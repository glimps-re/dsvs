package netns

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	mtu = 1500
	nic = 1

	// A UDP conversation has no end: it is closed when nothing goes through anymore.
	udpIdle = 30 * time.Second
)

// allowed says what is allowed out, and is called before each real connection: everything goes out,
// except the virtual network's private addresses — nothing legitimate looks for them outside, and
// following them there would leak the address plan onto the host.
func allowed(dst netip.AddrPort) bool {
	return !private.Contains(dst.Addr())
}

// private is set when the way out is set up, when the subnet is known; resolver and tun are set at
// the same moment. All three only concern the gatekeeper, which has only one way out to hold, and a
// negative tun says it is not connected yet.
var (
	private  netip.Prefix
	resolver Resolver
	tun      = -1
)

// relay connects the private network's TUN to a userspace network stack and returns: connections
// are handled as they come, each in its goroutine.
func relay(fd int) error {
	if err := unix.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("the TUN refuses non-blocking mode: %w", err)
	}
	tun = fd
	endpoint, err := fdbased.New(&fdbased.Options{FDs: []int{fd}, MTU: mtu})
	if err != nil {
		return fmt.Errorf("network stack: %w", err)
	}

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	if e := s.CreateNIC(nic, endpoint); e != nil {
		return fmt.Errorf("stack interface: %v", e)
	}
	// The stack accepts everything that arrives, whatever the destination: that is what lets it
	// intercept connections not addressed to it.
	s.SetPromiscuousMode(nic, true)
	s.SetSpoofing(nic, true)
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nic},
		{Destination: header.IPv6EmptySubnet, NIC: nic},
	})

	forwarder := tcp.NewForwarder(s, 0, 1024, func(r *tcp.ForwarderRequest) {
		// The destination is read before anything else: Complete invalidates the request.
		dst := where(r.ID())
		var queue waiter.Queue
		endpoint, e := r.CreateEndpoint(&queue)
		if e != nil {
			r.Complete(true)
			return
		}
		r.Complete(false)
		go pipe(gonet.NewTCPConn(&queue, endpoint), dst)
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, forwarder.HandlePacket)

	datagrams := udp.NewForwarder(s, func(r *udp.ForwarderRequest) {
		dst := where(r.ID())
		var queue waiter.Queue
		endpoint, e := r.CreateEndpoint(&queue)
		if e != nil {
			return
		}
		go pipe(gonet.NewUDPConn(&queue, endpoint), dst)
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, datagrams.HandlePacket)
	return nil
}

// pipe replays an intercepted connection in the normal world and copies the bytes both ways. The
// destination is the one the app targeted: neither the name, nor the certificate, nor anything of
// the content is touched.
func pipe(virtual net.Conn, dst netip.AddrPort) {
	defer virtual.Close()
	_, datagram := virtual.(*gonet.UDPConn)
	network := "tcp"
	if datagram {
		network = "udp"
	}
	// DNS is not relayed, it is resolved, and so it does not go out: the address the app
	// targeted only exists in the private network, and it is the machine's stack that knows how
	// to answer.
	if dst.Port() == dnsPort {
		serveDNS(virtual, !datagram, resolver, watchers.line)
		return
	}
	if !allowed(dst) {
		watchers.line(fmt.Sprintf("%-4s %-45s refused", network, dst))
		return
	}

	real, err := net.DialTimeout(network, dst.String(), 10*time.Second)
	if err != nil {
		watchers.line(fmt.Sprintf("%-4s %-45s %v", network, dst, err))
		return
	}
	defer real.Close()
	if datagram {
		// A datagram does not announce its end: without exchange, the conversation is over.
		_ = virtual.SetDeadline(time.Now().Add(udpIdle))
		_ = real.SetDeadline(time.Now().Add(udpIdle))
	}
	watchers.line(fmt.Sprintf("%-4s %-45s open", network, dst))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyThenTell(real, virtual) }()
	go func() { defer wg.Done(); copyThenTell(virtual, real) }()
	wg.Wait()
}

// copyThenTell copies one direction of the conversation then tells the other end there will be
// nothing more: without this half-close, a client waiting for the end of the response would wait
// forever.
func copyThenTell(to, from net.Conn) {
	_, _ = io.Copy(to, from)
	if half, ok := to.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
		return
	}
	to.SetReadDeadline(time.Now())
}

// where returns the destination the app targeted: in an intercepted connection, it is the local
// address as the stack sees it.
func where(id stack.TransportEndpointID) netip.AddrPort {
	addr, _ := netip.AddrFromSlice(id.LocalAddress.AsSlice())
	return netip.AddrPortFrom(addr.Unmap(), id.LocalPort)
}

// watchers carries the connections through which the supervisor follows what goes out. The
// gatekeeper has no interface of its own: it tells, and it is the supervisor that logs.
var watchers = &audience{}

type audience struct {
	mu    sync.Mutex
	conns []net.Conn
}

// hold keeps a connection open until the other end closes it.
func (a *audience) hold(conn net.Conn) error {
	a.mu.Lock()
	a.conns = append(a.conns, conn)
	a.mu.Unlock()
	// Reading to the end keeps the connection open and tells when it closes.
	_, err := io.Copy(io.Discard, conn)
	a.drop(conn)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (a *audience) drop(conn net.Conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, held := range a.conns {
		if held == conn {
			a.conns = append(a.conns[:i], a.conns[i+1:]...)
			return
		}
	}
}

// line sends a line to whoever listens. Nobody listening is a normal case: the gatekeeper also runs
// before the supervisor has opened its window.
func (a *audience) line(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, conn := range a.conns {
		_, _ = fmt.Fprintln(conn, strings.TrimRight(text, "\n"))
	}
}
