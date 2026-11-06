package netns

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

// A request the gatekeeper does not know returns a readable refusal rather than blocking the
// supervisor.
func TestGateRefusesWhatItCannotGive(t *testing.T) {
	t.Parallel()
	ctl := gate(t)

	conn, err := net.Dial("unix", ctl)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "open everything for me\n")
	answer, _ := io.ReadAll(conn)
	if !strings.Contains(string(answer), "unknown request") {
		t.Errorf("answer = %q, want an explicit refusal", answer)
	}
}

// Outside the systemd box, the gatekeeper refuses to move to the binary on disk: its exec would
// kill the supervisor, which Pdeathsig then guards against orphans.
func TestGateKeepsItsBinaryOutsideTheBox(t *testing.T) {
	t.Parallel()
	ctl := gate(t)

	conn, err := net.Dial("unix", ctl)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "reload\n")
	answer, _ := io.ReadAll(conn)
	if !strings.Contains(string(answer), "no systemd box") {
		t.Errorf("answer = %q, want a refusal that names the box", answer)
	}
}

// What the gatekeeper carries across its exec reads back as is, and what is not of that shape is
// refused rather than taking over a network nobody knows.
func TestWhatTheGateHandsOverSurvivesTheTrip(t *testing.T) {
	t.Parallel()
	sent := crossing{tun: 7, listen: 12, pid: 4231, subnet: netip.MustParsePrefix("10.66.0.0/24")}

	back, err := parseCrossing(sent.String())
	if err != nil || back != sent {
		t.Errorf("read back = %+v (err %v), sent %+v", back, err, sent)
	}
	for _, wrong := range []string{"", "7,12,4231", "7,12,4231,10.66.0.0", "a,12,4231,10.66.0.0/24"} {
		if _, err := parseCrossing(wrong); err == nil {
			t.Errorf("%q accepted", wrong)
		}
	}
}

// gate sets up the control socket and the gatekeeper behind it, without namespace or box: the
// protocol does not depend on where it is called from.
func gate(t *testing.T) string {
	t.Helper()
	ctl := filepath.Join(t.TempDir(), "ctl")
	listener, err := net.Listen("unix", ctl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go (&gatekeeper{listener: listener}).answer()
	return ctl
}

// The address plan is deterministic and only depends on the subnet: the first address stays with
// the gatekeeper, the apps follow in the order they were declared.
func TestThePlanIsStable(t *testing.T) {
	t.Parallel()
	subnet := netip.MustParsePrefix("10.77.0.0/24")

	for rank, want := range []string{"10.77.0.2", "10.77.0.3", "10.77.0.4"} {
		got, err := nth(subnet, rank)
		if err != nil || got.String() != want {
			t.Errorf("address %d = %v (err %v), want %s", rank, got, err, want)
		}
	}
	// Another subnet is configured to avoid a VPN, without anything else changing.
	if addr, err := nth(netip.MustParsePrefix("192.168.240.0/29"), 1); err != nil || addr.String() != "192.168.240.3" {
		t.Errorf("address in another subnet = %v (err %v)", addr, err)
	}
}

// The plan stops before the resolver's address and before the end of the subnet: one app too many
// is refused its address rather than sharing another's.
func TestThePlanEndsSomewhere(t *testing.T) {
	t.Parallel()
	subnet := netip.MustParsePrefix("10.77.0.0/29")

	// .0 network, .1 gatekeeper, .2 to .5 apps, .6 resolver, .7 broadcast.
	if _, err := nth(subnet, 3); err != nil {
		t.Errorf("the fourth app fits in %s: %v", subnet, err)
	}
	if addr, err := nth(subnet, 4); err == nil {
		t.Errorf("address %v given although %s is full", addr, subnet)
	}
}
