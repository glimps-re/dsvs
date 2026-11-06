package netns

import (
	"context"
	"net"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"dsvs/internal/config"
)

const freshNamespace = "DSVS_TEST_IN_FRESH_NAMESPACE"

// The interfaces are followed over netlink, directly as through the gatekeeper. The test writes
// into the network — it creates links — and therefore only does so in a throwaway namespace: the
// parent re-launches itself under unshare -rn, and the child refuses to write if it does not find a
// new network there.
func TestTheLinksAreFollowed(t *testing.T) {
	if os.Getenv(freshNamespace) == "" {
		if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
			t.Skipf("no user and network namespace to write in (%v: %s)", err, out)
		}
		child := exec.Command("unshare", "-rn", os.Args[0], "-test.run=^TestTheLinksAreFollowed$", "-test.v")
		child.Env = append(os.Environ(), freshNamespace+"=1")
		if out, err := child.CombinedOutput(); err != nil {
			t.Fatalf("in the fresh namespace: %v\n%s", err, out)
		}
		return
	}
	requireFreshNamespace(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	say := func(text string) { t.Log(text) }

	t.Run("read here", func(t *testing.T) {
		values, err := Observe(ctx, "", say)
		if err != nil {
			t.Fatal(err)
		}
		walk(t, values)
		withoutAddress(t, values)
		withoutCarrier(t, values)
		walkExits(t, values)
	})
	t.Run("read by the gate", func(t *testing.T) {
		values, err := Observe(ctx, gate(t), say)
		if err != nil {
			t.Fatal(err)
		}
		walk(t, values)
		walkExits(t, values)
	})
}

// An up link that only has its link-local address changes nothing in what the machine shows.
func withoutAddress(t *testing.T, values <-chan config.Observed) {
	t.Helper()
	bare := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "bare0"}}
	if err := netlink.LinkAdd(bare); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(bare)
	if err := netlink.LinkSetUp(bare); err != nil {
		t.Fatal(err)
	}
	select {
	case seen := <-values:
		t.Fatalf("a link without an address produced %+v", seen)
	case <-time.After(quiet + time.Second):
	}
}

// An up link without a carrier — a veth whose peer is down, like docker0 without a container — is
// not an interface the machine goes out through.
func withoutCarrier(t *testing.T, values <-chan config.Observed) {
	t.Helper()
	uncabled := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "veth0"}, PeerName: "veth1"}
	if err := netlink.LinkAdd(uncabled); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(uncabled)
	addr, err := netlink.ParseAddr("203.0.113.5/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(uncabled, addr); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(uncabled); err != nil {
		t.Fatal(err)
	}
	select {
	case seen := <-values:
		t.Fatalf("a link without a carrier produced %+v", seen)
	case <-time.After(quiet + time.Second):
	}
}

// walkExits changes the default gateway without touching the link or the address — what a NetVM
// change does — and expects a single value each time.
func walkExits(t *testing.T, values <-chan config.Observed) {
	t.Helper()
	eth := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}
	raise := func() {
		t.Helper()
		addr, err := netlink.ParseAddr("192.0.2.17/32")
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkAdd(eth); err != nil {
			t.Fatal(err)
		}
		if err := netlink.AddrAdd(eth, addr); err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(eth); err != nil {
			t.Fatal(err)
		}
	}
	raise()
	defer func() {
		netlink.LinkDel(eth)
		expect(t, values, config.Observed{Links: []config.Link{}, Exits: []config.Exit{}})
	}()
	links := []config.Link{{Name: "eth0", Type: "dummy", Addrs: []string{"192.0.2.17/32"}}}
	exitsVia := func(gateway string) config.Observed {
		return config.Observed{Links: links, Exits: []config.Exit{{Via: gateway, Dev: "eth0"}}}
	}
	through := func(gateway string, write func(*netlink.Route) error) {
		t.Helper()
		ip := net.ParseIP(gateway)
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: eth.Index, Dst: &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}, Scope: netlink.SCOPE_LINK}); err != nil {
			t.Fatal(err)
		}
		if err := write(&netlink.Route{LinkIndex: eth.Index, Gw: ip, Flags: int(netlink.FLAG_ONLINK)}); err != nil {
			t.Fatal(err)
		}
	}
	expect(t, values, config.Observed{Links: links, Exits: []config.Exit{}})

	through("198.51.100.1", netlink.RouteAdd)
	expect(t, values, exitsVia("198.51.100.1"))

	through("198.51.100.2", netlink.RouteReplace)
	expect(t, values, exitsVia("198.51.100.2"))
	silence(t, values)

	def := &netlink.Route{LinkIndex: eth.Index, Gw: net.ParseIP("198.51.100.2"), Flags: int(netlink.FLAG_ONLINK)}
	if err := netlink.RouteDel(def); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := netlink.RouteAdd(def); err != nil {
		t.Fatal(err)
	}
	silence(t, values)

	through("203.0.113.9", netlink.RouteReplace)
	expect(t, values, exitsVia("203.0.113.9"))

	// The Qubes-style recreation: the link disappears, long enough for udev to come, then comes
	// back with its routes. A single value comes out of it, the final one.
	if err := netlink.LinkDel(eth); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	raise()
	through("198.51.100.1", netlink.RouteReplace)
	expect(t, values, exitsVia("198.51.100.1"))
	silence(t, values)
}

func silence(t *testing.T, values <-chan config.Observed) {
	t.Helper()
	select {
	case seen := <-values:
		t.Fatalf("one value too many: %+v", seen)
	case <-time.After(quiet + 500*time.Millisecond):
	}
}

// walk makes a link appear then disappear, and reads what the observation says about it.
func walk(t *testing.T, values <-chan config.Observed) {
	t.Helper()
	expect(t, values, config.Observed{Links: []config.Link{}, Exits: []config.Exit{}})

	tun := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "tun0"}}
	if err := netlink.LinkAdd(tun); err != nil {
		t.Fatal(err)
	}
	addr, err := netlink.ParseAddr("10.8.0.2/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(tun, addr); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(tun); err != nil {
		t.Fatal(err)
	}
	expect(t, values, config.Observed{Links: []config.Link{{Name: "tun0", Type: "dummy", Addrs: []string{"10.8.0.2/24"}}}, Exits: []config.Exit{}})

	if err := netlink.LinkDel(tun); err != nil {
		t.Fatal(err)
	}
	expect(t, values, config.Observed{Links: []config.Link{}, Exits: []config.Exit{}})
}

func expect(t *testing.T, values <-chan config.Observed, want config.Observed) {
	t.Helper()
	select {
	case got := <-values:
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("observed %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("nothing observed within 2 s, want %+v", want)
	}
}

// requireFreshNamespace is the guard that keeps a handling mistake from writing into the machine's
// network: only lo exists, and it is down.
func requireFreshNamespace(t *testing.T) {
	t.Helper()
	links, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].Name != "lo" || links[0].Flags&net.FlagUp != 0 {
		t.Fatalf("this is not a new namespace, nothing is written: %+v", links)
	}
}
