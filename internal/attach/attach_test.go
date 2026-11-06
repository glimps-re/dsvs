package attach

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"dsvs/internal/config"
	"dsvs/internal/core"
	"dsvs/internal/fdpass"
	"dsvs/internal/hub"
)

// An interface attaches on the terminal the client lends, and the session is what writes on it: none
// of the state crosses the socket, only descriptors.
func TestAttachRendersOnTheLentTerminal(t *testing.T) {
	session, path := session(t)
	server := bind(t, path)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	server.Ready(ctx, session, stop)

	screen := lend(t)
	done := make(chan error, 1)
	go func() { done <- Attach(path, "plain", nil, refuse) }()

	// Streamed logs flush on the session's events: keep talking until heard, like an app writing while
	// the client attaches.
	said := session.LogSink(session.Log)
	talking := make(chan struct{})
	defer close(talking)
	go func() {
		for {
			select {
			case <-talking:
				return
			case <-time.After(100 * time.Millisecond):
				said("the session's log")
			}
		}
	}()

	await(t, screen, "the session's log")

	// The interface leaves with the session, and the client learns it without having to exit with an
	// error.
	server.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the client exited on: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the client did not return")
	}
}

// The socket is the session's voice: a client that arrived before it serves learns there that it did
// not start, rather than waiting in silence.
func TestAttachLearnsWhyTheSessionDidNotStart(t *testing.T) {
	_, path := session(t)
	server := bind(t, path)
	screen := lend(t)

	go server.Fail(errors.New("port 4390 already taken"))

	err := Attach(path, "plain", nil, refuse)
	if err == nil || !strings.Contains(err.Error(), "port 4390 already taken") {
		t.Errorf("the client got %v, want the session's reason", err)
	}
	// The terminal was never taken: restoring the alternate screen there moves the cursor to where
	// nothing was saved, and the failure's reason would be written over the terminal's first line.
	if seen := glance(t, screen); seen != "" {
		t.Errorf("the client wrote %q on a terminal the session did not take", seen)
	}
}

// A session that dies mid-interface leaves the alternate screen on and the cursor hidden: the client
// must restore them, otherwise the shell comes back in an unusable terminal.
func TestAttachGivesTheScreenBackWhenTheSessionDies(t *testing.T) {
	_, path := session(t)
	sock, err := hub.Bind(socket(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sock.Close() })
	go func() {
		conn, err := sock.Accept()
		if err != nil {
			return
		}
		_, fds, _ := fdpass.Recv(conn.(*net.UnixConn))
		shut(fds)
		fmt.Fprintln(conn, "ok")
		conn.Close()
	}()

	screen := lend(t)
	if err := Attach(path, "plain", nil, refuse); err == nil {
		t.Error("the client exited without an error although the session broke the link")
	}
	if seen := glance(t, screen); !strings.Contains(seen, "\x1b[?1049l") {
		t.Errorf("the client wrote %q, want the alternate screen restored", seen)
	}
}

// An invalid configuration reaches the client as the session read it: the file's path and the cause,
// on a single line when there is only one fault. That is all the terminal will get.
func TestAttachSaysWhyTheConfigurationIsInvalid(t *testing.T) {
	_, path := session(t)
	if err := os.WriteFile(path, []byte(`{"apps": [{"name": "front", "command": ["serve", "--port", "{port}"]}], "ingress": [{"routes": [{"path": "/", "app": "front"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, invalid := config.Load(path, nil, config.Input{})
	if invalid == nil {
		t.Fatal("this configuration should be refused")
	}
	server := bind(t, path)
	screen := lend(t)
	go server.Fail(invalid)

	err := Attach(path, "plain", nil, refuse)
	if err == nil || err.Error() != invalid.Error() {
		t.Errorf("the client got %q, the session read %q", err, invalid)
	}
	if strings.Count(err.Error(), "\n") > 0 {
		t.Errorf("a single fault fits on one line, the client read %q", err)
	}
	if seen := glance(t, screen); seen != "" {
		t.Errorf("the client wrote %q on a terminal the session did not take", seen)
	}
}

// A driver reads the session's state and gives it orders, without showing anything or lending a
// terminal: that is how the panel holds it.
func TestWatchReadsTheStateAndGivesOrders(t *testing.T) {
	session, path := running(t)
	server := bind(t, path)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	server.Ready(ctx, session, stop)

	link, err := Watch(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { link.Close() })

	first := <-link.States()
	if first.Config != path || len(first.Apps) != 1 || first.Apps[0].Name != "sleeper" {
		t.Fatalf("first state = %+v", first)
	}
	if first.Apps[0].Status != "idle" {
		t.Errorf("the app should be stopped, it is %q", first.Apps[0].Status)
	}

	if err := link.Order("start", "sleeper"); err != nil {
		t.Fatal(err)
	}
	until(t, link, func(s State) bool { return s.Apps[0].PID > 0 })

	if err := link.Order("stop", "sleeper"); err != nil {
		t.Fatal(err)
	}
	until(t, link, func(s State) bool { return s.Apps[0].Status == "idle" && s.Apps[0].Last != "" })
}

// until waits for a state that satisfies this condition: the session sends a fresh one on every
// change, and the latest evicts the one waiting.
func until(t *testing.T, link *Link, ready func(State) bool) State {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case state, live := <-link.States():
			if !live {
				t.Fatal("the session dropped the link")
			}
			if ready(state) {
				return state
			}
		case <-deadline:
			t.Fatal("the expected state never came")
		}
	}
}

// A refusal's reason reaches the driver whole, although its stream is framed by line ends: it is what
// the panel shows on the project's line, and a path without its cause says nothing.
func TestWatchLearnsTheWholeReason(t *testing.T) {
	_, path := session(t)
	server := bind(t, path)
	go server.Fail(errors.New("config.dsvs.ts:\n  - no app declared\n  - missing port"))

	_, err := Watch(path)
	if err == nil {
		t.Fatal("the driver got a link to a session that gave up")
	}
	for _, want := range []string{"config.dsvs.ts", "no app declared", "missing port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the driver read %q, without %q", err, want)
		}
	}
}

// The logs verb returns the tail of the logs without attaching anything: the session's, and those of
// the apps the names bring along — all of them with no name. It is what a terminal that does not want
// the stream reads.
func TestTailReadsTheEndOfTheJournals(t *testing.T) {
	session, path := running(t)
	server := bind(t, path)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	server.Ready(ctx, session, stop)

	session.Log.Add("the session speaks")
	session.App("sleeper").Log.Add("the app speaks")

	logs, err := Tail(path, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !said(logs, "the session speaks") || !said(logs, "the app speaks") {
		t.Errorf("with no name, the tail of the logs should hold everything; read %v", logs)
	}

	// Naming an app keeps only its log, with the session's, which tells what surrounds it.
	logs, err = Tail(path, 50, []string{"sleeper"})
	if err != nil {
		t.Fatal(err)
	}
	if !said(logs, "the app speaks") || !said(logs, "the session speaks") {
		t.Errorf("naming sleeper should keep its log and the session's; read %v", logs)
	}

	// The line count is a log tail: the last lines, not the first.
	for i := range 5 {
		session.Log.Addf("line %d", i)
	}
	logs, err = Tail(path, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || !said(logs, "line 3") || !said(logs, "line 4") {
		t.Errorf("-n 2 should return the last two lines; read %v", logs)
	}

	// A name the configuration does not know is an error that says so, as for an order.
	if _, err := Tail(path, 50, []string{"ghost"}); err == nil ||
		!strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %v; want the refused name", err)
	}
}

// Two renewals, two verbs: reload makes the session reread the configuration, renew makes it take the
// binary on disk. Mixing them up made a session asked to reread its configuration exec itself
// instead.
func TestReloadAndRenewAreTwoOrders(t *testing.T) {
	session, path := running(t)
	server := bind(t, path)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	server.Ready(ctx, session, stop)

	link, err := Watch(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { link.Close() })

	// No handover is set up on this session: renew is refused with a message saying so, and that is what
	// shows the verb arrived safely.
	if err := link.Order("renew"); err != nil {
		t.Fatal(err)
	}
	told(t, session, "binary on disk")

	// This test's configuration does not come from a file: rereading it fails, and the log says so —
	// again, the proof that the verb arrived.
	if err := link.Order("reload"); err != nil {
		t.Fatal(err)
	}
	told(t, session, filepath.Base(path))
}

// told waits for this text in the session's log: an order arrives through the socket, and what it
// yields is written in a goroutine other than the test's.
func told(t *testing.T, session *core.Session, text string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, line := range session.Log.Lines() {
			if strings.Contains(line.Text, text) {
				return
			}
		}
	}
	t.Fatalf("the session's log never carried %q; it says %v", text, session.Log.Lines())
}

// said says whether these lines carry this text.
func said(logs []Log, text string) bool {
	for _, line := range logs {
		if strings.Contains(line.Text, text) {
			return true
		}
	}
	return false
}

// The socket carries the configuration's path, which is the session's identity: two configurations
// never meet, the same one finds itself again.
func TestTheSocketFollowsTheConfiguration(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	one, two := "/one/config.dsvs.ts", "/two/config.dsvs.ts"
	if socket(one) == socket(two) {
		t.Error("two configurations share a socket")
	}
	if socket(one) != socket(one) {
		t.Error("the same configuration changes socket")
	}
}

func session(t *testing.T) (*core.Session, string) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	cfg := config.Config{Dir: dir, Path: filepath.Join(dir, "config.dsvs.json")}
	return core.NewSession(config.Form{}, cfg, core.Loopback{}, noFront{}, func(*core.Session) core.Gateway { return nil }), cfg.Path
}

// running sets up a supervised session, with an app that sleeps: enough to observe an order.
func running(t *testing.T) (*core.Session, string) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	cfg := config.Config{
		Dir:  dir,
		Path: filepath.Join(dir, "config.dsvs.json"),
		Port: 4399,
		Apps: []config.App{
			{Name: "sleeper", Command: []string{"sh", "-c", "sleep 30"}, Dir: dir},
		},
		Ingresses: []config.Ingress{config.NewIngress(config.Ingress{Host: "shop.localhost"})},
	}
	session := core.NewSession(config.Form{}, cfg, core.Loopback{}, noFront{}, func(*core.Session) core.Gateway { return idleGateway{} })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		session.Run(ctx, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return session, cfg.Path
}

func bind(t *testing.T, path string) *Server {
	t.Helper()
	server, err := Bind(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

// lend replaces the process's terminal with a pseudo-terminal, and returns the end that reads what is
// written on it. Attach lends the process's terminal: that is the whole point, so it is the one a test
// must divert. A real terminal and not a pipe, because what is observed here — whether the terminal's
// state was restored — only exists on a terminal.
func lend(t *testing.T) *os.File {
	t.Helper()
	screen, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { screen.Close(); tty.Close() })
	in, out := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = tty, tty
	t.Cleanup(func() { os.Stdin, os.Stdout = in, out })
	return screen
}

// glance returns everything the client wrote on the lent terminal, including nothing. A marker written
// after it bounds the read: a terminal does not close, and nothing else would say there is nothing left
// to read.
func glance(t *testing.T, screen *os.File) string {
	t.Helper()
	const mark = "[end]"
	fmt.Fprint(os.Stdout, mark)
	var seen []byte
	buf := make([]byte, 256)
	for {
		if before, _, found := bytes.Cut(seen, []byte(mark)); found {
			return string(before)
		}
		n, err := screen.Read(buf)
		seen = append(seen, buf[:n]...)
		if err != nil {
			t.Fatalf("the marker did not come back from the lent terminal; read %q: %v", seen, err)
		}
	}
}

// await waits for this line on the lent terminal. The read lives in its own goroutine: a silent pipe
// makes the test fail instead of blocking it.
func await(t *testing.T, screen *os.File, want string) {
	t.Helper()
	lines, found, seen := bufio.NewReader(screen), make(chan struct{}), make(chan string, 64)
	go func() {
		for {
			line, err := lines.ReadString('\n')
			seen <- line
			if strings.Contains(line, want) {
				close(found)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-found:
	case <-time.After(5 * time.Second):
		var lines []string
		for len(seen) > 0 {
			lines = append(lines, <-seen)
		}
		t.Fatalf("%q did not come on the lent terminal; read: %q", want, lines)
	}
}

// refuse is a test's session launcher: the session is already there, nobody has to launch one.
func refuse() error { return errors.New("no session to launch here") }

type idleGateway struct{}

func (idleGateway) Start(context.Context) error { return nil }
func (idleGateway) Stop(context.Context) error  { return nil }
func (idleGateway) Stats(string) (int, int)     { return 0, 0 }

type noFront struct{}

func (noFront) Listen(int) (net.Listener, error) { return nil, errors.New("no port in this test") }
func (noFront) Hosts([]string) error             { return nil }
