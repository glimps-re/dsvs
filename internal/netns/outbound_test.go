package netns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// A connection leaving the private network fails as on a real network, and right away: a server
// that refuses is reported refused, a destination the machine does not reach is reported
// unreachable. The relay used to blur them into a connection accepted then closed, and the proxy
// could no longer tell them apart.
//
// The test writes into the network: the parent re-launches itself under unshare -rn, as for the
// links. In this namespace, the normal world, the relay holds the TUN of a second namespace, the
// apps', which only a locked thread inhabits.
func TestTheRelayFailsAsARealNetworkWould(t *testing.T) {
	if os.Getenv(freshNamespace) == "" {
		if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
			t.Skipf("no user and network namespace to write in (%v: %s)", err, out)
		}
		child := exec.Command("unshare", "-rn", os.Args[0], "-test.run=^TestTheRelayFailsAsARealNetworkWould$", "-test.v")
		child.Env = append(os.Environ(), freshNamespace+"=1")
		if out, err := child.CombinedOutput(); err != nil {
			t.Fatalf("in the fresh namespace: %v\n%s", err, out)
		}
		return
	}
	requireFreshNamespace(t)

	// The normal world: a stopped server on 192.0.2.80, and 203.0.113.0/24 without a route.
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	must(t, netlink.LinkSetUp(lo))
	must(t, give(lo, "192.0.2.80"))
	must(t, netlink.RouteAdd(&netlink.Route{Dst: prefix("203.0.113.0/24"), Type: unix.RTN_UNREACHABLE}))

	type dialed struct {
		err  error
		took time.Duration
	}
	asked, answered := make(chan string), make(chan dialed)
	go func() {
		// This thread becomes the apps' network, and stays so: it never leaves it, and the
		// connections leave from it.
		runtime.LockOSThread()
		defer close(answered)
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			t.Error(err)
			return
		}
		file, err := openTUN("tun0")
		if err == nil {
			err = wireForTest(int(file.Fd()))
		}
		if err != nil {
			t.Error(err)
			return
		}
		for addr := range asked {
			began := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			cancel()
			if err == nil {
				conn.Close()
			}
			answered <- dialed{err, time.Since(began)}
		}
	}()
	defer close(asked)

	for _, c := range []struct {
		addr string
		want error
	}{
		{"192.0.2.80:80", unix.ECONNREFUSED},
		{"203.0.113.80:80", unix.EHOSTUNREACH},
	} {
		select {
		case asked <- c.addr:
		case <-answered:
			t.FailNow()
		}
		got, ok := <-answered
		if !ok {
			t.FailNow()
		}
		if !errors.Is(got.err, c.want) || got.took > time.Second {
			t.Errorf("%s: %v in %s, want %v right away", c.addr, got.err, got.took, c.want)
		}
	}
}

// wireForTest sets up, in the calling thread's namespace, what Join sets up for the apps: lo, the
// TUN up with an address, and the default route that leads to it. The relay takes the other end.
func wireForTest(fd int) error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	tunnel, err := netlink.LinkByName("tun0")
	if err != nil {
		return err
	}
	for _, step := range []func() error{
		func() error { return netlink.LinkSetUp(lo) },
		func() error { return give(tunnel, "10.77.0.2") },
		func() error { return netlink.LinkSetUp(tunnel) },
		func() error {
			return netlink.RouteAdd(&netlink.Route{LinkIndex: tunnel.Attrs().Index, Dst: prefix("0.0.0.0/0")})
		},
		func() error { return relay(fd) },
	} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func prefix(text string) *net.IPNet {
	p := netip.MustParsePrefix(text)
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
