package netns

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"dsvs/internal/core"
	"dsvs/internal/fdpass"
)

// outLink is the name of the way-out interface, as seen from inside the private network.
const outLink = "out0"

const (
	// CtlEnv carries the control socket's path. Its presence tells the binary it is the supervisor,
	// already in its namespace: it is the only switch between the two roles.
	CtlEnv = "DSVS_NETNS_CTL"

	// Unreachable is the code by which the supervisor says it could not set up the private network.
	// The gatekeeper then falls back to loopback mode, keeping the reason.
	Unreachable = 78
)

// Gate plays the gatekeeper's role: it sets up the control socket, re-executes the binary in a user
// and network namespace, serves its requests, and returns the supervisor's exit code. An error says
// the namespace is out of reach — it is up to the caller to fall back to loopback.
func Gate() (int, error) {
	dir, err := os.MkdirTemp("", "dsvs-")
	if err != nil {
		return 0, err
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "ctl"))
	if err != nil {
		os.RemoveAll(dir)
		return 0, err
	}

	child := exec.Command("/proc/self/exe", os.Args[1:]...)
	child.Env = append(os.Environ(), CtlEnv+"="+listener.Addr().String())
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  unix.CLONE_NEWUSER | unix.CLONE_NEWNET | unix.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		Pdeathsig:   syscall.SIGKILL,
	}
	if err := child.Start(); err != nil {
		listener.Close()
		os.RemoveAll(dir)
		return 0, err
	}
	return (&gatekeeper{listener: listener, child: child.Process}).hold()
}

// gatekeeper is what the gatekeeper process holds: the control socket, and the supervisor it waits
// for. The way out is in the package's variables — there is only one.
type gatekeeper struct {
	listener net.Listener
	child    *os.Process
}

// hold serves the supervisor until its death and returns its exit code. The control socket's
// directory leaves with the gatekeeper's last image: an exec runs no defer, so it is always the one
// that really waits for the supervisor that cleans up.
func (g *gatekeeper) hold() (int, error) {
	defer os.RemoveAll(filepath.Dir(g.listener.Addr().String()))
	defer g.listener.Close()

	// The terminal belongs to the supervisor: ctrl+c is a key for it, not a signal for the
	// gatekeeper. A deliberate stop, though, must get through: it is relayed and waited for.
	signal.Ignore(os.Interrupt)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range stop {
			_ = g.child.Signal(sig)
		}
	}()
	go g.answer()

	state, err := g.child.Wait()
	if err != nil {
		return 0, err
	}
	return state.ExitCode(), nil
}

// answer serves the supervisor's requests until the socket closes: one per connection, which avoids
// any interleaving.
func (g *gatekeeper) answer() {
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			// The request is read by recvmsg: an ordinary read would drop the descriptor that comes
			// with it, and the TUN arrives that way.
			request, fds, err := fdpass.Recv(conn.(*net.UnixConn))
			if err != nil {
				return
			}
			if err := g.grant(conn.(*net.UnixConn), request, fds); err != nil {
				fmt.Fprintf(conn, "err %v\n", err)
			}
		}()
	}
}

// grant carries out a request in the normal world. The supervisor can only ask for the connection
// of its way out, whose descriptor it gives, and for a way to follow it. Nothing else crosses.
func (g *gatekeeper) grant(conn *net.UnixConn, request string, fds []int) error {
	verb, rest, _ := strings.Cut(request, " ")
	switch verb {
	case "tun":
		if len(fds) != 1 {
			return errors.New("no TUN descriptor in the request")
		}
		subnet, err := netip.ParsePrefix(rest)
		if err != nil {
			return fmt.Errorf("unreadable subnet %q", rest)
		}
		private = subnet
		resolver = forwardTo(nameservers())
		if err := relay(fds[0]); err != nil {
			return err
		}
		_, err = fmt.Fprintln(conn, "ok")
		return err
	case "watch":
		// The connection stays open: the gatekeeper writes on it what goes out, until the
		// supervisor leaves.
		return watchers.hold(conn)
	}
	return fmt.Errorf("unknown request %q", verb)
}

// Virtual is the private network as the supervisor sees it: each app has its address there, and the
// public ports come from the gatekeeper.
type Virtual struct {
	ctl      string
	subnet   netip.Prefix
	warnings []string

	// mu guards the address plan, which grows during the session: an app declared by a reload takes
	// the next address. An assigned address is never taken back — the table also says what an app
	// had, and the subnet holds more than enough.
	mu  sync.Mutex
	lo  netlink.Link
	ips map[string]string
}

// Join sets up the private network — lo up, one address per app — and returns the network the
// session consumes. To be called only inside the namespace, that is when CtlEnv is there.
func Join(ctl string, apps []string, subnet netip.Prefix) (*Virtual, error) {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return nil, fmt.Errorf("lo not found: %w", err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return nil, fmt.Errorf("lo does not come up: %w", err)
	}

	v := &Virtual{ctl: ctl, subnet: subnet, lo: lo, ips: map[string]string{}}
	for _, name := range apps {
		if _, err := v.reserve(name); err != nil {
			return nil, err
		}
	}

	// The way out: a TUN whose other end the gatekeeper holds, and the default route that leads to
	// it. As long as it is not connected, nothing can leave the private network.
	if err := v.wire(); err != nil {
		return nil, err
	}
	// DNS goes through the same way out, provided the apps look for it inside. Without this mount,
	// they keep the machine's resolv.conf: it works if it carries an address reachable from
	// outside, not if it points to a local resolver.
	if err := privateResolv(filepath.Dir(ctl), v.resolverAddr()); err != nil {
		v.warnings = append(v.warnings, fmt.Sprintf("!! resolv.conf of the private network: %v", err))
	}
	return v, nil
}

// gateway is the gatekeeper's address as seen from inside: the one it gives to the way-out
// interface.
func (v *Virtual) gateway() netip.Addr { return v.subnet.Addr().Next() }

// resolverAddr is the address where the apps look for their DNS: the last usable one of the subnet,
// that of no interface. That is what matters — an address local to the namespace would be delivered
// on the spot, whereas the query must go up into the TUN.
func (v *Virtual) resolverAddr() netip.Addr { return resolverAddr(v.subnet) }

func resolverAddr(subnet netip.Prefix) netip.Addr {
	base := subnet.Masked().Addr().As4()
	last := binary.BigEndian.Uint32(base[:]) + uint32(1)<<(32-subnet.Bits()) - 2
	var bytes [4]byte
	binary.BigEndian.PutUint32(bytes[:], last)
	return netip.AddrFrom4(bytes)
}

// Warnings says what could not be set up without preventing the session for all that: the network's
// log collects them at start.
func (v *Virtual) Warnings() []string { return v.warnings }

// wire creates the way-out interface, sends there everything that does not stay home, and gives its
// descriptor to the gatekeeper. The gatekeeper reads it, from the normal world: a descriptor stays
// attached to the namespace it was born in, which is the whole trick.
func (v *Virtual) wire() error {
	out, err := openTUN(outLink)
	if err != nil {
		return err
	}
	defer out.Close()

	link, err := netlink.LinkByName(outLink)
	if err != nil {
		return fmt.Errorf("%s not found: %w", outLink, err)
	}
	// The interface takes the subnet's first address, with its mask: the apps keep priority on
	// their own, which they carry as /32, and everything else goes through here.
	gateway := netip.PrefixFrom(v.gateway(), v.subnet.Bits())
	addr, err := netlink.ParseAddr(gateway.String())
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("%s refused on %s: %w", gateway, outLink, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("%s does not come up: %w", outLink, err)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
	}); err != nil {
		return fmt.Errorf("outbound route: %w", err)
	}

	return v.ask("tun "+v.subnet.String(), int(out.Fd()))
}

// ask carries a request to the gatekeeper and returns what it says about it. The attached
// descriptors travel with it: that is how the TUN gets across.
func (v *Virtual) ask(request string, fds ...int) error {
	conn, err := net.Dial("unix", v.ctl)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := fdpass.Send(conn.(*net.UnixConn), request, fds...); err != nil {
		return err
	}
	answer, _, err := fdpass.Recv(conn.(*net.UnixConn))
	if err != nil {
		return err
	}
	if answer != "ok" {
		return errors.New(strings.TrimPrefix(answer, "err "))
	}
	return nil
}

// Watch follows what leaves the private network: the gatekeeper tells, the supervisor logs. The
// connection stays open until the end of the session.
func (v *Virtual) Watch(line func(string)) {
	conn, err := net.Dial("unix", v.ctl)
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, "watch"); err != nil {
		return
	}
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line(scanner.Text())
	}
}

// openTUN opens a tunnel interface in the namespace: the packets that enter it come out in the
// clear in the descriptor, without a protocol header.
func openTUN(name string) (*os.File, error) {
	file, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("no tunnel possible: %w", err)
	}
	request, err := unix.NewIfreq(name)
	if err != nil {
		return nil, err
	}
	request.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(int(file.Fd()), unix.TUNSETIFF, request); err != nil {
		file.Close()
		return nil, fmt.Errorf("creating %s: %w", name, err)
	}
	return file, nil
}

// nth returns the plan's n-th address, n counting from zero: the subnet's first stays with the
// gatekeeper, for the way-out route, and the last usable one with the resolver. Two sessions of the
// same configuration therefore give the same address plan.
func nth(subnet netip.Prefix, n int) (netip.Addr, error) {
	addr := subnet.Masked().Addr().Next()
	for range n + 1 {
		addr = addr.Next()
	}
	if !subnet.Contains(addr) || addr == resolverAddr(subnet) {
		return netip.Addr{}, fmt.Errorf("%s does not hold %d apps", subnet, n+1)
	}
	return addr, nil
}

// Assign gives the app its virtual address and a free port on it. Two apps can therefore carry the
// same port number without getting in each other's way. An app the network does not know is an app
// a reload just declared: it takes the plan's next address.
func (v *Virtual) Assign(app string) (core.Endpoint, error) {
	ip, err := v.reserve(app)
	if err != nil {
		return core.Endpoint{}, err
	}
	port, err := core.FreePort(ip)
	return core.Endpoint{IP: ip, Port: port}, err
}

// reserve returns the app's address, giving it one if it has none yet: it is the only place where
// the plan grows, and where an address arrives on lo.
func (v *Virtual) reserve(app string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ip, known := v.ips[app]; known {
		return ip, nil
	}
	addr, err := nth(v.subnet, len(v.ips))
	if err != nil {
		return "", err
	}
	ip := addr.String()
	if err := give(v.lo, ip); err != nil {
		return "", err
	}
	v.ips[app] = ip
	return ip, nil
}

// Dial reaches an app from the namespace, where the proxy runs: an ordinary local connection.
func (v *Virtual) Dial(ctx context.Context, ep core.Endpoint) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", ep.String())
}

func (v *Virtual) Mode() core.Mode {
	return core.Mode{
		Name:     "netns",
		Isolated: true,
	}
}

func give(link netlink.Link, ip string) error {
	// Each app takes its address as /32: the rest of the subnet stays routable elsewhere, which the
	// transparent way out will need.
	addr, err := netlink.ParseAddr(ip + "/32")
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("%s refused: %w", ip, err)
	}
	return nil
}
