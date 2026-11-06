package hub

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dsvs/internal/core"
	"dsvs/internal/fdpass"
)

// The hub passes the connection to the session that owns the host: the session serves an ordinary
// request, knowing nothing of the way it arrived.
func TestHubHandsTheConnectionToTheOwnerOfTheHost(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	if body, code := get(t, port, "one.test"); body != "session one" || code != 200 {
		t.Errorf("response = %d %q, want 200 \"session one\"", code, body)
	}
}

// Two sessions on the same port: each receives only its own hosts, which is the hub's whole reason
// to be.
func TestHubKeepsTwoSessionsApartOnOnePort(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")
	serve(t, port, "/two/config.dsvs.ts", []string{"two.test", "more.test"}, "session two")

	for host, want := range map[string]string{"one.test": "session one", "two.test": "session two", "more.test": "session two"} {
		if body, code := get(t, port, host); body != want || code != 200 {
			t.Errorf("%s = %d %q, want 200 %q", host, code, body, want)
		}
	}
}

// The host nobody claims gets a 503 that says what is served here: in front of it, the only
// question one asks is which name should have been typed.
func TestHubTellsWhatItServesWhenTheHostIsUnknown(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	body, code := get(t, port, "nowhere.test")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", code)
	}
	if !strings.Contains(body, "one.test") {
		t.Errorf("the 503 says %q, want it to name one.test", body)
	}
}

// A facade without a host takes what nobody claims: it is what the configuration says, and the hub
// must be able to tell it apart from a session that declares nothing at all.
func TestHubGivesTheUnclaimedHostsToTheFacadeThatHasNone(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")
	serve(t, port, "/all/config.dsvs.ts", []string{""}, "the rest")

	if body, code := get(t, port, "one.test"); body != "session one" || code != 200 {
		t.Errorf("one.test = %d %q, want 200 \"session one\"", code, body)
	}
	if body, code := get(t, port, "nowhere.test"); body != "the rest" || code != 200 {
		t.Errorf("nowhere.test = %d %q, want 200 \"the rest\"", code, body)
	}
}

// A session without a facade receives nothing: declaring nothing is not taking everything.
func TestHubGivesNothingToASessionThatDeclaresNoHost(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/silent/config.dsvs.ts", nil, "nobody")

	if _, code := get(t, port, "nowhere.test"); code != http.StatusServiceUnavailable {
		t.Errorf("nowhere.test = %d, want 503", code)
	}
}

// A session that leaves releases its hosts: nothing to take back, nothing to reconcile.
func TestHubForgetsTheHostsOfASessionThatLeaves(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/stays/config.dsvs.ts", []string{"stays.test"}, "I stay")
	leaving := serve(t, port, "/gone/config.dsvs.ts", []string{"gone.test"}, "I leave")

	leaving.Close()
	waitFor(t, func() bool { _, code := get(t, port, "gone.test"); return code == http.StatusServiceUnavailable })

	if body, code := get(t, port, "stays.test"); body != "I stay" || code != 200 {
		t.Errorf("the remaining session answers %d %q", code, body)
	}
}

// The hosts are announced whole every time: a reload that adds one gets it served without the port
// moving.
func TestHubTakesTheHostsAgainOnEveryAnnounce(t *testing.T) {
	startHub(t)
	port := freePort(t)
	front := serveFront(t, port, "/one/config.dsvs.ts", []string{"before.test"}, "session")

	if err := front.Hosts([]string{"before.test", "after.test"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, code := get(t, port, "after.test"); return code == 200 })
}

// The hub holds as many ports as it is asked for, and releases each with its last session: that is
// what tells a hub of ports apart from a hub of one port.
func TestHubHoldsSeveralPortsAndClosesEachWithItsLastSession(t *testing.T) {
	startHub(t)
	one, two := freePort(t), freePort(t)
	leaving := serve(t, one, "/one/config.dsvs.ts", []string{"one.test"}, "session one")
	serve(t, two, "/two/config.dsvs.ts", []string{"two.test"}, "session two")

	if body, code := get(t, two, "two.test"); body != "session two" || code != 200 {
		t.Fatalf("the second port answers %d %q", code, body)
	}
	leaving.Close()
	waitFor(t, func() bool {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", one))
		if err == nil {
			conn.Close()
		}
		return err != nil
	})
	if body, code := get(t, two, "two.test"); body != "session two" || code != 200 {
		t.Errorf("the second port answers %d %q after the first one closed", code, body)
	}
}

// A host has only one server per port: the second session that declares it is refused at
// registration, and the refusal names the configuration that holds it. Without that, its requests
// would go to its neighbor.
func TestHubRefusesAHostAlreadyServedOnThePort(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test", "shared.test"}, "session one")

	front := New("/two/config.dsvs.ts", false)
	if err := front.Hosts([]string{"shared.test"}); err != nil {
		t.Fatal(err)
	}
	_, err := front.Listen(port)
	if err == nil {
		t.Fatal("the second session was registered, want a refusal")
	}
	if !strings.Contains(err.Error(), "shared.test") || !strings.Contains(err.Error(), "/one/config.dsvs.ts") {
		t.Errorf("refusal = %q, want it to name the host and the configuration that holds it", err)
	}
	if body, code := get(t, port, "shared.test"); body != "session one" || code != 200 {
		t.Errorf("the contested host answers %d %q, want it to stay with its owner", code, body)
	}
}

// A configuration runs only once: it is a session's identity, and two sessions of the same file are
// not a representable state.
func TestHubRefusesASecondSessionOfTheSameConfig(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	front := New("/one/config.dsvs.ts", false)
	if err := front.Hosts([]string{"other.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := front.Listen(freePort(t)); err == nil {
		t.Fatal("the second session of the same file was registered, want a refusal")
	} else if !strings.Contains(err.Error(), "/one/config.dsvs.ts") {
		t.Errorf("refusal = %q, want it to name the configuration", err)
	}
}

// A reload that claims another session's host does not get it, and learns so: it runs, it is not
// stopped for a contested name, but it must not believe it serves it.
func TestHubKeepsAContestedHostWithItsOwnerOnReload(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")
	two := serveFront(t, port, "/two/config.dsvs.ts", []string{"two.test"}, "session two")

	if err := two.Hosts([]string{"two.test", "one.test"}); err != nil {
		t.Fatal(err)
	}
	news := awaitNews(t, two, "one.test")
	if !strings.Contains(news, "/one/config.dsvs.ts") {
		t.Errorf("news = %q, want it to name the configuration that holds the host", news)
	}
	if body, code := get(t, port, "one.test"); body != "session one" || code != 200 {
		t.Errorf("the contested host answers %d %q, want it to stay with its owner", code, body)
	}
	if body, code := get(t, port, "two.test"); body != "session two" || code != 200 {
		t.Errorf("the session's own host answers %d %q, want it still served", code, body)
	}
}

// The hub is the only one to know which sessions are alive: it is through it that a panel finds
// them, and without it the answer is "none", not an error.
func TestSessionsListsWhatLives(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if living, err := Sessions(); err != nil || len(living) != 0 {
		t.Fatalf("without a hub: %v, %v", living, err)
	}

	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	living, err := Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(living) != 1 || living[0].Config != "/one/config.dsvs.ts" || living[0].Port != port {
		t.Errorf("sessions = %+v, want the session registered on port %d", living, port)
	}
}

// A client that closes without having written its headers gets its response at once: nothing keeps
// running waiting for a rest that will never come.
func TestHubAnswersAtOnceWhenTheRequestEndsBeforeItsHeaders(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	conn := dial(t, port)
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	answer, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("no response within 500 ms: %v", err)
	}
	if !strings.Contains(string(answer), "400 Bad Request") {
		t.Errorf("response = %q, want a 400", answer)
	}
}

// A client that opens and never writes ends up dropped: the public port does not let itself be
// robbed of a goroutine and a descriptor per silent connection.
func TestHubDropsAConnectionThatNeverSendsAnything(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	conn := dial(t, port)
	conn.SetReadDeadline(time.Now().Add(peekPatience + time.Second))
	answer, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("silent connection never dropped: %v", err)
	}
	if !strings.Contains(string(answer), "400 Bad Request") {
		t.Errorf("response = %q, want a 400", answer)
	}
}

// Only one hub is born, however many sessions ask for one together over a dead one's socket: the
// others step aside without a word, and the one that stays serves.
func TestOnlyOneDispatcherIsBorn(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	port := freePort(t)
	if err := os.WriteFile(Addr(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const rush = 64
	back := make(chan error, rush)
	for range rush {
		go func() { back <- Serve() }()
	}
	for range rush - 1 {
		select {
		case err := <-back:
			if err != nil {
				t.Fatalf("a hub returned %v, want it to step aside without a word", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("more than one hub is there")
		}
	}

	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")
	if body, code := get(t, port, "one.test"); body != "session one" || code != 200 {
		t.Errorf("the port answers %d %q, want 200 \"session one\"", code, body)
	}
}

// A hub that accepts then stops without answering: the session says what happened, where an ended
// stream says nothing by itself.
func TestSessionSaysTheDispatcherStoppedWithoutAnswering(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	port := freePort(t)
	// Nothing must be born: it is the hub in place that is queried, until the session gives up
	// finding one that answers.
	previous := spawnHub
	spawnHub = func(bool) error { return nil }
	t.Cleanup(func() { spawnHub = previous })
	sock, err := net.Listen("unix", Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sock.Close() })
	go func() {
		for {
			conn, err := sock.Accept()
			if err != nil {
				return
			}
			fdpass.NewReader(conn.(*net.UnixConn)).Line()
			conn.Close()
		}
	}()

	if _, err := New("/one/config.dsvs.ts", false).Listen(port); err == nil {
		t.Fatal("the session believed itself registered")
	} else if !strings.Contains(err.Error(), "without answering") {
		t.Errorf("error = %q, want it to tell the hub's stop", err)
	}
}

// The hub that dies does not take the session with it: its services keep running, a new hub takes
// the place, and the port serves again without anybody having restarted anything.
func TestSessionTakesItsPortBackWhenTheDispatcherDies(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	port := freePort(t)
	hubs := testHubsOf(t, port)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	if body, code := get(t, port, "one.test"); body != "session one" || code != 200 {
		t.Fatalf("before the hub's death: %d %q", code, body)
	}
	hubs.kill()
	waitFor(t, func() bool {
		body, code, err := ask(port, "one.test")
		return err == nil && code == 200 && body == "session one"
	})
}

// A session that closes during the revival does not reappear: its close is final, even when caught
// between a hub's death and the next one's arrival.
func TestASessionClosedDuringTheHandoverStaysGone(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	port := freePort(t)
	hubs := testHubsOf(t, port)
	serve(t, port, "/stays/config.dsvs.ts", []string{"stays.test"}, "I stay")
	leaving := serve(t, port, "/gone/config.dsvs.ts", []string{"gone.test"}, "I leave")

	hubs.hold()
	hubs.kill()
	leaving.Close()
	hubs.release()

	waitFor(t, func() bool {
		body, code, err := ask(port, "stays.test")
		return err == nil && code == 200 && body == "I stay"
	})
	waitFor(t, func() bool {
		_, code, err := ask(port, "gone.test")
		return err == nil && code == http.StatusServiceUnavailable
	})
	time.Sleep(200 * time.Millisecond)
	if _, code := get(t, port, "gone.test"); code != http.StatusServiceUnavailable {
		t.Errorf("gone.test = %d, want it to stay at 503", code)
	}
}

// A hub of another version refuses the session and says what to do: two binaries that do not speak
// the same language do not half-understand each other.
func TestDispatcherRefusesAnotherVersionOfTheProtocol(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	conn, err := net.Dial("unix", Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "hello %d %d other.test /other/config.dsvs.ts\n", Version+1, port); err != nil {
		t.Fatal(err)
	}
	answer, err := fdpass.NewReader(conn.(*net.UnixConn)).Line()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(answer, "err ") || !strings.Contains(answer, strconv.Itoa(Version)) {
		t.Errorf("response = %q, want a refusal that tells the protocol version", answer)
	}
	if _, code := get(t, port, "other.test"); code != http.StatusServiceUnavailable {
		t.Errorf("other.test = %d, want the refused session to serve nothing", code)
	}
}

// A hub that no longer swallows what is written to it no longer freezes the session: the
// declaration returns at its deadline, and the close does not wait behind it.
func TestSessionIsNotFrozenByADispatcherThatStopsReading(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	port := freePort(t)
	deaf(t)

	front := New("/one/config.dsvs.ts", false)
	if err := front.Hosts([]string{"one.test"}); err != nil {
		t.Fatal(err)
	}
	listener, err := front.Listen(port)
	if err != nil {
		t.Fatal(err)
	}

	// The socket fills up, then the declaration waits, then it gives up: it is this deadline that
	// is measured, the previous one did not exist.
	long := []string{"one.test", strings.Repeat("x", 200) + ".test"}
	var refused error
	for stop := time.Now().Add(20 * time.Second); refused == nil && time.Now().Before(stop); {
		refused = front.Hosts(long)
	}
	if refused == nil {
		t.Fatal("the hub's socket never overflowed")
	}

	closed := make(chan error, 1)
	go func() { closed <- listener.Close() }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the session's close waits for a hub that no longer reads")
	}
}

// A hub that welcomes the session then dies at once, in a loop, does not make it spin: the revival
// has its pace, and the log does not fill up with one line per round.
func TestSessionDoesNotSpinOnADispatcherThatDiesAtOnce(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	port := freePort(t)
	greetings := fleeting(t)

	front := New("/one/config.dsvs.ts", false)
	if err := front.Hosts([]string{"one.test"}); err != nil {
		t.Fatal(err)
	}
	listener, err := front.Listen(port)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, err := listener.Accept(); err != nil {
				return
			}
		}
	}()

	time.Sleep(4 * reviveEvery / 3)
	listener.Close()
	switch taken := greetings.Load(); {
	case taken < 2:
		t.Errorf("%d registrations: the session did not take its port back", taken)
	case taken > 4:
		t.Errorf("%d registrations in %v: the revival spins", taken, 4*reviveEvery/3)
	}
}

// What is not an HTTP request gets its response on the first segment: a TLS ClientHello on the
// public port no longer keeps the hub busy for two seconds.
func TestHubAnswersAtOnceWhenTheFirstSegmentIsNotHTTP(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	conn := dial(t, port)
	clientHello := []byte{0x16, 0x03, 0x01, 0x00, 0x2f, 0x01, 0x00, 0x00, 0x2b, 0x03, 0x03}
	if _, err := conn.Write(clientHello); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	answer, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("no response within 500 ms: %v", err)
	}
	if !strings.Contains(string(answer), "400 Bad Request") {
		t.Errorf("response = %q, want a 400", answer)
	}
}

// A client that starts its request then goes silent gets its 400: the response goes out on a socket
// from which what was only peeked at was first swallowed, otherwise it closes with an RST and the
// client reads nothing of what was written to it.
func TestHubAnswersAClientThatFallsSilentMidRequest(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	conn := dial(t, port)
	if _, err := conn.Write([]byte("GET")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(peekPatience + 2*peekStepMax))
	answer, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("no response to the client that went silent: %v", err)
	}
	if !strings.Contains(string(answer), "400 Bad Request") {
		t.Errorf("response = %q, want a 400", answer)
	}
}

// A client that writes without ever stopping is dropped as fast as any other: the hub only swallows
// what it had peeked at, it does not follow a stream the public port can make last forever.
func TestHubDoesNotFollowAClientThatKeepsWriting(t *testing.T) {
	startHub(t)
	port := freePort(t)
	serve(t, port, "/one/config.dsvs.ts", []string{"one.test"}, "session one")

	conn := dial(t, port)
	done := make(chan struct{})
	defer close(done)
	go func() {
		flood := make([]byte, 4096)
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := conn.Write(flood); err != nil {
				return
			}
		}
	}()

	conn.SetReadDeadline(time.Now().Add(time.Second))
	answer, err := io.ReadAll(conn)
	// What is left behind the swallowed buffer makes the connection end with an RST: the response
	// can therefore be lost, but the close must come.
	if err != nil && os.IsTimeout(err) {
		t.Fatalf("the endlessly writing client received nothing within a second (read %q)", answer)
	}
	if len(answer) > 0 && !strings.Contains(string(answer), "400 Bad Request") {
		t.Errorf("response = %q, want a 400 or a close", answer)
	}
}

func TestStartsRequest(t *testing.T) {
	cases := []struct {
		name  string
		head  string
		start bool
	}{
		{"a request", "GET / HTTP/1.1\r\n", true},
		{"a WebDAV method", "PROPFIND / HTTP/1.1\r\n", true},
		{"WebDAV's most verbose", "BASELINE-CONTROL / HTTP/1.1\r\n", true},
		{"a TLS ClientHello", "\x16\x03\x01\x00/", false},
		{"a lowercase method", "get / HTTP/1.1\r\n", false},
		{"an endless word", strings.Repeat("A", longestMethod+1), false},
		{"too early to decide", "GE", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if start := startsRequest([]byte(c.head)); start != c.start {
				t.Errorf("startsRequest = %v, want %v", start, c.start)
			}
		})
	}
}

func TestHostInHeaders(t *testing.T) {
	cases := []struct {
		name  string
		head  string
		host  string
		found bool
	}{
		{"plain host", "GET / HTTP/1.1\r\nHost: one.test\r\n\r\n", "one.test", true},
		{"port ignored", "GET / HTTP/1.1\r\nHost: one.test:4200\r\n\r\n", "one.test", true},
		{"case ignored", "GET / HTTP/1.1\r\nhOsT: One.TEST\r\n\r\n", "one.test", true},
		{"after other headers", "GET / HTTP/1.1\r\nAccept: */*\r\nHost: one.test\r\n\r\n", "one.test", true},
		{"without a host", "GET / HTTP/1.0\r\nAccept: */*\r\n\r\n", "", true},
		{"bare line feed", "GET / HTTP/1.1\nHost: one.test\n\n", "one.test", true},
		{"bare line feed, without a host", "GET / HTTP/1.0\n\n", "", true},
		{"incomplete headers", "GET / HTTP/1.1\r\nHos", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, found := hostIn([]byte(c.head))
			if host != c.host || found != c.found {
				t.Errorf("hostIn = %q, %v; want %q, %v", host, found, c.host, c.found)
			}
		})
	}
}

// startHub launches a hub on a free port, in a session directory of its own.
func startHub(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	// A socket file lies around in the session directory: it is what a dead hub leaves, and every
	// test therefore goes through its replacement.
	if err := os.WriteFile(Addr(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve() }()
	t.Cleanup(func() {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the hub stopped on: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("the hub did not leave with its last session")
		}
	})
	waitFor(t, func() bool {
		conn, err := net.Dial("unix", Addr())
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
}

// serve registers a session that answers its name to everything, and returns its listener.
func serve(t *testing.T, port int, config string, hosts []string, answer string) net.Listener {
	t.Helper()
	return serveFront(t, port, config, hosts, answer).listener
}

// testFront is the session's network, the listener it obtained and what the hub told it, kept
// together for the duration of the test.
type testFront struct {
	*Front
	listener net.Listener
	news     chan string
}

func serveFront(t *testing.T, port int, config string, hosts []string, answer string) *testFront {
	t.Helper()
	front := New(config, false)
	news := make(chan string, 8)
	front.Say(func(line string) { news <- line })
	if err := front.Hosts(hosts); err != nil {
		t.Fatal(err)
	}
	listener, err := front.Listen(port)
	if err != nil {
		t.Fatalf("the session did not join the hub: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, answer)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Shutdown(context.Background()) })
	return &testFront{Front: front, listener: listener, news: news}
}

// awaitNews waits for the news about this subject: the hub tells others, and nothing says the
// awaited one arrives first.
func awaitNews(t *testing.T, front *testFront, about string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case line := <-front.news:
			if strings.Contains(line, about) {
				return line
			}
		case <-deadline:
			t.Fatalf("no news from the hub about %s", about)
		}
	}
}

// get asks a host for a page. A new connection every time: the hub chooses the session when the
// connection opens, and a client that kept its own for another host would stay with the session it
// was handed to — which is what a browser does, whose connection pool is per origin.
func get(t *testing.T, port int, host string) (string, int) {
	t.Helper()
	body, code, err := ask(port, host)
	if err != nil {
		t.Fatalf("%s: %v", host, err)
	}
	return body, code
}

// ask is the same request, when the port may not answer: between a hub's death and the next one,
// nobody holds the port and the connection is refused.
func ask(port int, host string) (string, int, error) {
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	if err != nil {
		return "", 0, err
	}
	request.Host = host
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := client.Do(request)
	if err != nil {
		return "", 0, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return strings.TrimSpace(string(body)), res.StatusCode, nil
}

// waitFor waits for a condition to become true: the hub and the sessions talk through a socket, and
// nothing guarantees a declaration has arrived when the next line runs.
func waitFor(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("condition never reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// deaf holds the socket of a hub that welcomes the sessions then no longer swallows anything they
// write to it.
func deaf(t *testing.T) {
	t.Helper()
	sock, err := net.Listen("unix", Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sock.Close() })
	go func() {
		var kept []net.Conn
		defer func() {
			for _, conn := range kept {
				conn.Close()
			}
		}()
		for {
			conn, err := sock.Accept()
			if err != nil {
				return
			}
			kept = append(kept, conn)
			if _, err := fdpass.NewReader(conn.(*net.UnixConn)).Line(); err != nil {
				return
			}
			fmt.Fprintln(conn, "ok")
		}
	}()
}

// fleeting holds the socket of a hub that welcomes each session then leaves on the spot, and counts
// the registrations it welcomed.
func fleeting(t *testing.T) *atomic.Int64 {
	t.Helper()
	sock, err := net.Listen("unix", Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sock.Close() })
	var greetings atomic.Int64
	go func() {
		for {
			conn, err := sock.Accept()
			if err != nil {
				return
			}
			if _, err := fdpass.NewReader(conn.(*net.UnixConn)).Line(); err == nil {
				fmt.Fprintln(conn, "ok")
				greetings.Add(1)
			}
			conn.Close()
		}
	}()
	return &greetings
}

// dial opens a raw connection on the public port, for the cases an HTTP client cannot play.
func dial(t *testing.T, port int) *net.TCPConn {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn.(*net.TCPConn)
}

const hubEnv = "DSVS_TEST_HUB"

// TestDispatcherProcess is not a test: it is the hub itself, when the test binary is launched as
// such.
func TestDispatcherProcess(t *testing.T) {
	if os.Getenv(hubEnv) == "" {
		t.Skip("the test binary only serves as a hub on request")
	}
	if err := Serve(); err != nil {
		t.Fatal(err)
	}
}

// testHubs launches the test's hubs as processes that can be killed: the real launch re-executes
// the session's executable, which cannot be played from a test binary. The rest of the path is an
// ordinary session's. hold holds back the launches, which opens the window during which a session
// looks for a hub and has none; kill leaves none behind, otherwise a hub from the previous test
// would still hold a port for the next one.
type testHubs struct {
	t    *testing.T
	port int

	mu      sync.Mutex
	gate    chan struct{}
	running []*exec.Cmd
}

func testHubsOf(t *testing.T, port int) *testHubs {
	t.Helper()
	open := make(chan struct{})
	close(open)
	d := &testHubs{t: t, port: port, gate: open}
	previous := spawnHub
	spawnHub = func(bool) error { return d.start() }
	t.Cleanup(func() {
		spawnHub = previous
		d.kill()
	})
	return d
}

func (d *testHubs) start() error {
	d.mu.Lock()
	gate := d.gate
	d.mu.Unlock()
	<-gate

	d.mu.Lock()
	defer d.mu.Unlock()
	// Two sessions taking their port back each ask for one: the first launched serves both, where
	// the real launch would let the second see the place taken and leave — one more process for
	// nothing, and twice the time for the test.
	if len(d.running) > 0 {
		return nil
	}
	hub := exec.Command(os.Args[0], "-test.run=TestDispatcherProcess")
	hub.Env = append(os.Environ(), hubEnv+"=1")
	if err := hub.Start(); err != nil {
		return err
	}
	d.running = append(d.running, hub)
	return nil
}

// kill returns when nothing holds the port anymore: the next hub therefore starts from an empty
// place, as after the death of the only one that held it.
func (d *testHubs) kill() {
	d.mu.Lock()
	running := d.running
	d.running = nil
	d.mu.Unlock()
	for _, hub := range running {
		hub.Process.Kill()
		hub.Wait()
	}
	waitFor(d.t, func() bool {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", d.port))
		if err != nil {
			return true
		}
		conn.Close()
		return false
	})
}

func (d *testHubs) hold() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gate = make(chan struct{})
}

func (d *testHubs) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	close(d.gate)
}

func freePort(t *testing.T) int {
	t.Helper()
	port, err := core.FreePort("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return port
}
