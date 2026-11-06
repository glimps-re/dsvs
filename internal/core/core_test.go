package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"dsvs/internal/config"
	"dsvs/internal/projects"
)

// "sleep & wait" gives the shell a real child: a signal sent to the parent alone would leave it
// alive, and that is what the process group must prevent.
var sleeper = []string{"sh", "-c", "sleep 300 & wait"}

func TestLifecycle(t *testing.T) {
	t.Parallel()
	_, app, _ := start(t, sleeper)

	app.Start()
	waitFor(t, "ready", func() bool { return app.Snapshot().Status == Ready })
	first := app.Snapshot()
	if !app.IsReady() {
		t.Fatal("the app is ready but IsReady() says otherwise")
	}

	app.Restart()
	waitFor(t, "restarted", func() bool {
		snap := app.Snapshot()
		return snap.Status == Ready && snap.PID != first.PID
	})
	if got := app.Snapshot().Restarts; got != 1 {
		t.Errorf("restart count = %d, want 1", got)
	}
	if alive(first.PID) {
		t.Error("the previous run's process group survived the restart")
	}

	second := app.Snapshot().PID
	app.Stop()
	waitFor(t, "stopped", func() bool { return app.Snapshot().Status == Idle })
	last := app.Snapshot().Last
	switch {
	case last == nil:
		t.Fatal("no run outcome after a stop")
	case last.Ending != Stopped:
		t.Errorf("stop requested, outcome = %s", last)
	}
	if alive(second) {
		t.Error("the process group survived the stop")
	}
	if app.IsReady() {
		t.Error("the app is stopped but still says it is ready")
	}

	app.Start()
	waitFor(t, "started again", func() bool { return app.Snapshot().Status == Ready })
}

// Without a name on the command line, no app starts — the port, though, is already taken.
func TestNothingStartsUnasked(t *testing.T) {
	t.Parallel()
	session, app, gw := start(t, sleeper)
	wakeups, stopWatching := session.Watch()
	defer stopWatching()

	if !stays(wakeups, 200*time.Millisecond, func() bool {
		snap := app.Snapshot()
		return snap.Status == Idle && snap.Last == nil
	}) {
		t.Errorf("the app started on its own: %+v", app.Snapshot())
	}
	if !gw.listening() {
		t.Error("the port should be taken as soon as the session opens")
	}
}

// The port belongs to the session: taken once at opening, it crosses the apps' starts and stops and
// is only released at the end.
func TestThePortIsTakenForTheWholeSession(t *testing.T) {
	t.Parallel()
	_, app, gw := start(t, sleeper)

	app.Start()
	waitFor(t, "ready", func() bool { return app.Snapshot().Status == Ready })
	app.Stop()
	waitFor(t, "stopped", func() bool { return app.Snapshot().Status == Idle })

	if !gw.listening() {
		t.Error("the port was released while the session is still running")
	}
	if gw.started() != 1 || gw.stopped() != 0 {
		t.Errorf("gatekeeper started %d times, stopped %d times, want 1 and 0", gw.started(), gw.stopped())
	}
}

// ... and it is released when the session ends, once the apps have stopped.
func TestThePortIsReleasedWhenTheSessionEnds(t *testing.T) {
	t.Parallel()
	cfg, spy := fixture(t, sleeper)
	session := NewSession(config.Form{}, cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy })
	ctx, cancel := context.WithCancel(context.Background())
	if err := session.Open(ctx); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		session.Run(ctx, []string{"app"})
	}()
	waitFor(t, "ready", func() bool { return session.App("app").Snapshot().Status == Ready })

	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not stop")
	}
	if spy.stopped() != 1 {
		t.Errorf("gatekeeper stopped %d times, want 1", spy.stopped())
	}
	said := ""
	for _, line := range session.Log.Lines() {
		if line.Text != "" {
			said = line.Text
		}
	}
	if !strings.Contains(said, "port released") {
		t.Errorf("the session log should end on the released port, it ends on %q", said)
	}
}

// A busy port stops everything at start: it is an error one reads, not a log line in an interface
// that pretends to work.
func TestABusyPortStopsEverything(t *testing.T) {
	t.Parallel()
	cfg, spy := fixture(t, sleeper)
	spy.fail(errors.New("address already in use"))
	session := NewSession(config.Form{}, cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy })

	if err := session.Open(context.Background()); err == nil {
		t.Fatal("the session opened on a busy port")
	}
}

// The signal goes to the process group, not to the parent alone: a child that ignores the SIGHUP
// the terminal sends when its session leader dies must not survive for all that.
func TestStubbornChildDiesWithTheGroup(t *testing.T) {
	t.Parallel()
	_, app, _ := start(t, []string{"sh", "-c", "(trap '' HUP; sleep 300) & wait"})

	app.Start()
	waitFor(t, "ready", func() bool { return app.Snapshot().Status == Ready })
	group := app.Snapshot().PID
	app.Stop()
	waitFor(t, "stopped", func() bool { return app.Snapshot().Status == Idle })

	waitFor(t, "the process group emptied", func() bool { return !alive(group) })
}

// An app that exits on its own is not a stopped app: the interface must be able to tell them apart.
func TestCrashIsNotAStop(t *testing.T) {
	t.Parallel()
	_, app, _ := start(t, []string{"sh", "-c", "exit 3"})

	app.Start()
	waitFor(t, "exited", func() bool {
		snap := app.Snapshot()
		return snap.Status == Idle && snap.Last != nil
	})
	last := app.Snapshot().Last
	if last.Ending != ByItself || last.Code != 3 {
		t.Errorf("outcome = %+v, want an unrequested exit with code 3", last)
	}
}

// The log says how each run ends: a signal nobody asked for by its name, a requested restart as a
// restart and not as a death.
func TestTheJournalSaysHowARunEnded(t *testing.T) {
	t.Parallel()
	_, app, _ := start(t, sleeper)

	app.Start()
	waitFor(t, "ready", func() bool { return app.Snapshot().Status == Ready })
	app.Restart()
	waitFor(t, "restarted", func() bool { return said(app, "-- restarted") && app.Snapshot().Status == Ready })
	if said(app, "-- killed by SIGTERM") {
		t.Error("the requested restart is written as a death")
	}

	if err := syscall.Kill(-app.Snapshot().PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fallen", func() bool { return app.Snapshot().Status == Idle })
	if last := app.Snapshot().Last; last.String() != "killed by SIGKILL" || !last.Fell() {
		t.Errorf("end of run = %q (fell: %v), want killed by SIGKILL, fell", last, last.Fell())
	}
	if !said(app, "-- killed by SIGKILL") {
		t.Errorf("the log does not tell the crash: %v", app.Log.Lines())
	}
}

// An app that listens elsewhere than at its address stays "starting", and the status says where it
// listens: dsvs reads it from the sockets its process tree holds, here those of a node launched by
// a shell.
func TestAnAppListeningElsewhereSaysWhere(t *testing.T) {
	t.Parallel()
	cfg, spy := fixture(t, []string{"sh", "-c", `node -e 'require("http").createServer().listen(0, "127.0.0.1")'; :`, "{port}"})
	// An address where nothing listens, even a neighboring test that would take the port back:
	// loopback returns a free port on 127.0.0.1, which a test server can take in the meantime, and
	// the app would look ready.
	port, err := FreePort("127.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(config.Form{}, cfg, held{ep: Endpoint{IP: "127.0.0.2", Port: port}}, ownPort{}, func(*Session) Gateway { return spy })
	supervising(t, session)
	app := session.App("app")

	app.Start()
	waitFor(t, "the address where it listens", func() bool { return app.Snapshot().Astray != nil })
	snap := app.Snapshot()
	if on := snap.Astray.On; snap.Status != Starting || len(on) != 1 || !strings.HasPrefix(on[0], "127.0.0.1:") || on[0] == snap.Astray.Want.String() {
		t.Errorf("status = %s, %s; want starting, and the address where node listens", snap.Status, snap.Astray)
	}
	app.Stop()
	waitFor(t, "stopped", func() bool { return app.Snapshot().Status == Idle })
	if snap := app.Snapshot(); snap.Astray != nil {
		t.Errorf("a stopped app still says %s", snap.Astray)
	}
}

// What says an app is ready follows from its command when it declares nothing: an app that receives
// no port is ready as soon as it is launched, where it used to stay "starting" for lack of a port
// to probe.
func TestAnAppWithoutAPortIsReadyOnceLaunched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.Config{Port: 4300, Dir: dir, Apps: []config.App{{Name: "lib", Command: []string{"sleep", "300"}, Dir: dir}}}
	session := NewSession(config.Form{}, cfg, Loopback{}, ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	supervising(t, session)
	app := session.App("lib")

	app.Start()
	waitFor(t, "ready", func() bool { return app.Snapshot().Status == Ready })
}

// ready.log: the app is ready when a line of its output says so, once per launch — a previous run's
// line does not count for the next one.
func TestALineOfTheOutputSaysTheAppIsReady(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	once := filepath.Join(dir, "once")
	command := []string{"sh", "-c", fmt.Sprintf(`test -e %s || { touch %s; sleep 0.3; printf '\033[32mWatching\033[0m for file changes\n'; }; sleep 300`, once, once)}
	cfg := config.Config{Port: 4300, Dir: dir, Apps: []config.App{{Name: "lib", Command: command, Dir: dir,
		Ready: &config.Ready{Log: &config.Pattern{RegExp: `watching for`, Flags: "i"}}}}}
	session := NewSession(config.Form{}, cfg, Loopback{}, ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	supervising(t, session)
	app := session.App("lib")

	app.Start()
	waitFor(t, "starting", func() bool { return app.Snapshot().Status == Starting })
	waitFor(t, "ready when its output says so", func() bool { return app.Snapshot().Status == Ready })
	app.Restart()
	waitFor(t, "restarted", func() bool { return app.Snapshot().Restarts == 1 })
	time.Sleep(time.Second)
	if status := app.Snapshot().Status; status != Starting {
		t.Errorf("restarted without repeating the line, the app is %s, want starting", status)
	}
}

// ready.http: the app is ready on the first 2xx on this path, where a route's probe takes anything
// that is not a server error.
func TestAHealthPathWantsA2xx(t *testing.T) {
	t.Parallel()
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || !healthy.Load() {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	cfg, spy := fixture(t, []string{"sh", "-c", "sleep 300", "{port}"})
	cfg.Apps[0].Ready = &config.Ready{HTTP: "/health"}
	session := NewSession(config.Form{}, cfg, held{ep: Endpoint{IP: "127.0.0.1", Port: server.Listener.Addr().(*net.TCPAddr).Port}},
		ownPort{}, func(*Session) Gateway { return spy })
	supervising(t, session)
	app := session.App("app")

	app.Start()
	time.Sleep(time.Second)
	if status := app.Snapshot().Status; status != Starting {
		t.Fatalf("on a 404, the app is %s, want starting", status)
	}
	healthy.Store(true)
	waitFor(t, "ready on the first 2xx", func() bool { return app.Snapshot().Status == Ready })
}

// A command the queue can no longer take is refused and says so, instead of being lost. Without
// supervision running, nobody drains the queue: it fills up for sure.
func TestAFullQueueRefusesTheOrder(t *testing.T) {
	t.Parallel()
	cfg, spy := fixture(t, sleeper)
	app := NewSession(config.Form{}, cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy }).App("app")
	for range cap(app.cmds) {
		if err := app.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Stop(); err == nil || !strings.Contains(err.Error(), "app") {
		t.Errorf("Stop on a full queue = %v, want a refusal that names the app", err)
	}
}

func TestStartOnRunningAppChangesNothing(t *testing.T) {
	t.Parallel()
	session, app, _ := start(t, sleeper)
	wakeups, stopWatching := session.Watch()
	defer stopWatching()

	app.Start()
	waitFor(t, "ready", func() bool { return app.Snapshot().Status == Ready })
	pid := app.Snapshot().PID
	app.Start()
	app.Start()

	if !stays(wakeups, 200*time.Millisecond, func() bool {
		snap := app.Snapshot()
		return snap.PID == pid && snap.Status == Ready
	}) {
		t.Errorf("the app moved: %+v, want ready on pid %d", app.Snapshot(), pid)
	}
}

// The probe signals readiness only once: a channel left ready would make the supervisor spin and
// rewrite its state endlessly.
func TestReadyIsAnnouncedOnce(t *testing.T) {
	t.Parallel()
	session, app, _ := start(t, sleeper)
	wakeups, stopWatching := session.Watch()
	defer stopWatching()

	app.Start()
	waitFor(t, "ready", func() bool { return app.Snapshot().Status == Ready })
	first := app.Snapshot().ReadyAt

	if !stays(wakeups, 200*time.Millisecond, func() bool { return app.Snapshot().ReadyAt.Equal(first) }) {
		t.Errorf("the readiness instant moved (%s → %s): the state is republished in a loop", first, app.Snapshot().ReadyAt)
	}
}

// The last command wins: a stop requested during a restart cancels the restart.
func TestStopDuringRestartWins(t *testing.T) {
	t.Parallel()
	session, app, release := startLingering(t)
	wakeups, stopWatching := session.Watch()
	defer stopWatching()

	app.Restart()
	reaches(t, wakeups, app, Restarting)
	app.Stop()
	reaches(t, wakeups, app, Stopping)
	release()

	reaches(t, wakeups, app, Idle)
	if !stays(wakeups, 250*time.Millisecond, func() bool { return app.Snapshot().Status == Idle }) {
		t.Errorf("the app started again (%s) although a stop canceled the restart", app.Snapshot().Status)
	}
}

// A start received during the stop means "start again afterwards": it becomes a restart instead of
// being swallowed.
func TestStartDuringStopRestarts(t *testing.T) {
	t.Parallel()
	session, app, release := startLingering(t)
	wakeups, stopWatching := session.Watch()
	defer stopWatching()

	app.Stop()
	reaches(t, wakeups, app, Stopping)
	app.Start()
	release()

	waitFor(t, "started again once", func() bool {
		snap := app.Snapshot()
		return snap.Status == Ready && snap.Restarts == 1
	})
}

func fixture(t *testing.T, command []string) (config.Config, *spyGateway) {
	t.Helper()
	app := config.App{
		Name:    "app",
		Command: command,
		Dir:     t.TempDir(),
	}
	return config.Config{
		Port:      4300,
		Dir:       t.TempDir(),
		Ingresses: []config.Ingress{config.NewIngress(config.Ingress{Host: "localhost", Routes: []config.Route{{Path: "/app", App: "app"}}})},
		Apps:      []config.App{app},
	}, &spyGateway{}
}

// start sets up a session with one app and an ingress that serves it, without starting anything.
// The network is rigged: it gives the app the address of a listener the test already holds, which
// brings the app to Ready without a real server having to start.
func start(t *testing.T, command []string) (*Session, *App, *spyGateway) {
	t.Helper()
	cfg, spy := fixture(t, command)
	session := NewSession(config.Form{}, cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy })

	ctx, cancel := context.WithCancel(context.Background())
	if err := session.Open(ctx); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		session.Run(ctx, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("the session did not stop")
		}
	})
	return session, session.App("app"), spy
}

// startLingering returns a running service which, once the SIGTERM is received, stays dying until
// release is called: the transient states are held open long enough to assert them, instead of
// hanging on a window of a few hundred milliseconds that the machine's load closes. The wait also
// covers the other edge of that window: the probe declares the service ready without knowing
// anything of its process, which would die outright from a signal received before its trap.
//
// The trap blocks on reading a named pipe whose other end the test holds open from start to finish
// — O_RDWR, which does not block on a pipe without a reader. Nothing runs while it waits, whereas
// polling a marker file forked a sleep every twenty milliseconds: these tests thus produced the
// load they died from. And the byte waits for its reader, so release can come before the trap has
// opened the pipe without the order mattering.
func startLingering(t *testing.T) (session *Session, app *App, release func()) {
	t.Helper()
	dir := t.TempDir()
	hold, armed := filepath.Join(dir, "hold"), filepath.Join(dir, "armed")
	if err := syscall.Mkfifo(hold, 0o600); err != nil {
		t.Fatal(err)
	}
	holding, err := os.OpenFile(hold, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holding.Close() })
	session, app, _ = start(t, []string{"sh", "-c", fmt.Sprintf("trap 'read x < %s; exit 0' TERM; : > %s; sleep 300 & wait", hold, armed)})
	release = func() { _, _ = holding.WriteString("\n") }
	// Registered after start's cleanup, hence run before it: a test that fails while still holding
	// the transition lets the session stop all the same.
	t.Cleanup(release)

	app.Start()
	waitFor(t, "an app ready and armed against TERM", func() bool {
		return app.Snapshot().Status == Ready && exists(armed)
	})
	return session, app, release
}

// heldNet is a network that always gives the address of a server held by the test: the probe gets a
// real HTTP response there whatever the app does, and no test has to launch an application.
type held struct{ ep Endpoint }

func heldNet(t *testing.T) held {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(server.Close)
	addr := server.Listener.Addr().(*net.TCPAddr)
	return held{ep: Endpoint{IP: "127.0.0.1", Port: addr.Port}}
}

func (h held) Assign(string) (Endpoint, error) { return h.ep, nil }

func (h held) Dial(ctx context.Context, ep Endpoint) (net.Conn, error) {
	return Loopback{}.Dial(ctx, ep)
}

func (h held) Mode() Mode { return Mode{Name: "rigged"} }

// ownPort holds the port in place of the hub: a test does not have to launch one, and nothing opens
// there. The taken port is refused, like a port another program holds.
type ownPort struct{ taken int }

func (ownPort) Listen() (net.Listener, error) { return nil, errors.New("no port in this test") }

func (ownPort) Hosts([]string) error { return nil }

func (o ownPort) Move(port int) error {
	if port == o.taken {
		return errors.New("taken by another program")
	}
	return nil
}

// reaches waits for the app to reach this state, woken up by the session rather than polling it: a
// change that happened between two looks can no longer go unnoticed, and a state held open is
// asserted on the wake-up that announces it. That is what transient states need; a state where the
// app settles and stays, waitFor waits for just fine.
//
// The patience is gracePeriod, and that is the invariant: a transient state only lasts here as long
// as the supervisor lets a process that does not die on its SIGTERM live. Past that delay it
// escalates to SIGKILL, the transition ends without us, and waiting longer can no longer yield
// anything — might as well fail right away saying where the app got to.
func reaches(t *testing.T, wakeups <-chan Event, app *App, want Status) {
	t.Helper()
	patience := time.After(gracePeriod)
	for {
		if app.Snapshot().Status == want {
			return
		}
		select {
		case <-wakeups:
		case <-patience:
			t.Fatalf("the app is %s, want %s: the transition ended without us", app.Snapshot().Status, want)
		}
	}
}

// stays watches during window and says whether the condition never stopped being true. An absence
// is not awaited, it is observed: the session wakes its subscribers on every change, so each
// wake-up is inspected on the spot, and window does not say how long one hopes but how long one
// watches.
func stays(wakeups <-chan Event, window time.Duration, cond func() bool) bool {
	watching := time.After(window)
	for {
		select {
		case <-wakeups:
			if !cond() {
				return false
			}
		case <-watching:
			return cond()
		}
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// alive says whether the process group led by pid still counts anyone.
func alive(pid int) bool {
	return syscall.Kill(-pid, 0) == nil
}

// spyGateway replaces the proxy: it records what the session asks of it.
type spyGateway struct {
	mu       sync.Mutex
	starts   int
	stops    int
	up       bool
	failWith error
}

func (g *spyGateway) Start(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.starts++
	if g.failWith != nil {
		return g.failWith
	}
	g.up = true
	return nil
}

func (g *spyGateway) Stop(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stops++
	g.up = false
	return nil
}

func (g *spyGateway) Stats(string) (int, int) { return 0, 0 }

func (g *spyGateway) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failWith = err
}

func (g *spyGateway) listening() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.up
}

func (g *spyGateway) started() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.starts
}

func (g *spyGateway) stopped() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stops
}

// An app gets a real terminal: one can write to it, and what one types is echoed there, as in a
// shell.
func TestSendWritesToTheApp(t *testing.T) {
	t.Parallel()
	_, app, _ := start(t, []string{"sh", "-c", "read answer; echo \"received:$answer\"; sleep 30"})

	if err := app.Send("hello\r"); err == nil {
		t.Fatal("writing to a stopped app should fail")
	}
	app.Start()
	waitFor(t, "ready", func() bool { return app.Snapshot().Status == Ready })
	if err := app.Send("hello\r"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the app's answer", func() bool {
		for _, line := range app.Log.Lines() {
			if strings.Contains(line.Text, "received:hello") {
				return true
			}
		}
		return false
	})
	echoed := false
	for _, line := range app.Log.Lines() {
		if line.Text == "hello" {
			echoed = true
		}
	}
	if !echoed {
		t.Errorf("what one types should be echoed: %q", texts(app.Log.Lines()))
	}
}

// The size of the terminal lent to the app is the one it is given, and it holds for its next
// starts.
func TestResizeReachesTheApp(t *testing.T) {
	t.Parallel()
	_, app, _ := start(t, []string{"sh", "-c", "tput cols; sleep 30"})

	app.Resize(133, 42)
	app.Start()
	waitFor(t, "the announced width", func() bool { return said(app, "133") })
}

// Two interfaces watch the same session: the apps have only one terminal, and the smallest window
// decides. The one that leaves gives its room back to the others.
func TestTheSmallestViewportDecides(t *testing.T) {
	t.Parallel()
	session, app, _ := start(t, []string{"sh", "-c", "tput cols; sleep 30"})

	wide := session.Viewport()
	wide.Resize(133, 42)
	narrow := session.Viewport()
	narrow.Resize(80, 24)

	app.Start()
	waitFor(t, "the width of the smallest window", func() bool { return said(app, "80") })

	narrow.Close()
	app.Restart()
	waitFor(t, "the width regained after the small one left", func() bool { return said(app, "133") })
}

// Two interfaces watch the same session: each must be woken up. A single channel would make them
// steal each other's wake-ups, and one would redraw on the other's delay.
func TestEveryWatcherIsWokenUp(t *testing.T) {
	t.Parallel()
	session, app, _ := start(t, []string{"sh", "-c", "sleep 30"})
	one, releaseOne := session.Watch()
	two, releaseTwo := session.Watch()
	defer releaseTwo()

	app.Start()
	for who, events := range map[string]<-chan Event{"the first": one, "the second": two} {
		select {
		case <-events:
		case <-time.After(5 * time.Second):
			t.Errorf("%s interface was not woken up", who)
		}
	}

	// The released subscription closes, and whoever was waiting learns there is nothing left to
	// wait for.
	releaseOne()
	drained := make(chan struct{})
	go func() {
		for range one {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Error("the channel of a released subscription was not closed")
	}
}

// said says whether the app wrote this line, alone on its own.
func said(app *App, text string) bool {
	for _, line := range app.Log.Lines() {
		if strings.TrimSpace(line.Text) == text {
			return true
		}
	}
	return false
}

// An app learns its address through the tokens of its command: the network chose it, and the log
// shows the declared command, without a port nobody wrote.
func TestAppLearnsItsAddress(t *testing.T) {
	t.Parallel()
	_, app, _ := start(t, []string{"sh", "-c", "echo adresse={ip}:{port}; sleep 30"})

	app.Start()
	waitFor(t, "ready", func() bool { return app.Snapshot().Status == Ready })
	addr := app.Backend().Addr()

	waitFor(t, "the address announced by the app", func() bool {
		for _, line := range app.Log.Lines() {
			if strings.Contains(line.Text, "adresse="+addr) {
				return true
			}
		}
		return false
	})
	for _, line := range app.Log.Lines() {
		if strings.HasPrefix(line.Text, "$ ") && !strings.Contains(line.Text, "{port}") {
			t.Errorf("the log should show the declared command, not the resolved address: %q", line.Text)
		}
	}
}

// The proxy reaches an app only through its backend, which only exists while it answers.
func TestBackendLivesWithTheRun(t *testing.T) {
	t.Parallel()
	session, app, _ := start(t, sleeper)

	if session.Backend("app") != nil {
		t.Error("a stopped app should not have a backend")
	}
	app.Start()
	waitFor(t, "one backend", func() bool { return session.Backend("app") != nil })

	back := session.Backend("app")
	conn, err := back.DialContext(context.Background())
	if err != nil {
		t.Fatalf("the backend should open a connection: %v", err)
	}
	conn.Close()

	app.Stop()
	waitFor(t, "the backend withdrawn", func() bool { return session.Backend("app") == nil })
	if session.Backend("inconnu") != nil {
		t.Error("an app that does not exist should not have a backend")
	}
}

// An open port is not enough: where an ingress says through which path the app is served, the probe
// waits for a real response — a dev-server listens before it has compiled.
func TestReadinessAsksForARealAnswer(t *testing.T) {
	t.Parallel()
	compiling := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-compiling:
		default:
			// As long as nothing is compiled, the server listens but does not serve.
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	addr := server.Listener.Addr().(*net.TCPAddr)
	network := held{ep: Endpoint{IP: "127.0.0.1", Port: addr.Port}}

	declared := config.App{Name: "app", Command: sleeper, Dir: t.TempDir()}
	cfg := config.Config{
		Port:      4300,
		Dir:       t.TempDir(),
		Ingresses: []config.Ingress{config.NewIngress(config.Ingress{Host: "localhost", Routes: []config.Route{{Path: "/app", App: "app"}}})},
		Apps:      []config.App{declared},
	}
	session := NewSession(config.Form{}, cfg, network, ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); session.Run(ctx, []string{"app"}) }()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	app := session.App("app")
	wakeups, stopWatching := session.Watch()
	defer stopWatching()
	waitFor(t, "the app launched", func() bool { return app.Snapshot().Status == Starting })

	// At least two probe rounds: probeEvery decides how often questions are asked.
	if !stays(wakeups, 400*time.Millisecond, func() bool { return app.Snapshot().Status != Ready }) {
		t.Error("the app is announced ready while it answers 503")
	}

	close(compiling)
	waitFor(t, "ready once it serves", func() bool { return app.Snapshot().Status == Ready })
}

// The reload replaces what is declarative — facades, routes, targets, groups — and leaves what is
// alive. A facade that stays keeps its log; what the re-read configuration would like to change in
// the port or the apps is ignored, and said.
func TestReloadReplacesWhatIsDeclarative(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	declare := func(target, extra string) {
		t.Helper()
		body := `export default {
    port: 4300,
    ingress: [
        { host: "localhost", routes: [{ path: "/app", app: "app" }, { path: "/api", proxy: { target: "` + target + `" } }] },
        ` + extra + `
    ],
    apps: [{ name: "app", command: ["sh", "-c", "sleep 300 & wait", "{port}"] }],
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declare("https://first.example", "")
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	kept := session.Facades()[0]
	kept.Log.Add("a trace from before")

	declare("https://second.example", `{ host: "bis.localhost", routes: [{ path: "/", proxy: { target: "https://bis.example" } }] },`)
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}

	in, _ := session.Facade("localhost")
	route, _ := in.Match("/api/x")
	if route.Proxy.URL != "https://second.example" {
		t.Errorf("target after reload = %q, want the one just declared", route.Proxy.URL)
	}
	if got := session.Facades(); len(got) != 2 || got[1].Cfg.Name != "bis.localhost" {
		t.Fatalf("facades = %v, want the previous one and the new one", names(got))
	}
	if session.Facades()[0].Log != kept.Log {
		t.Error("the facade that stays got another log: what it had said would be lost")
	}
	if got := joinLines(session.Facades()[0].Log.Lines()); !strings.Contains(got, "a trace from before") {
		t.Errorf("the log of the kept facade was emptied:\n%s", got)
	}
	if got := session.Config().Port; got != 4300 {
		t.Errorf("port = %d, want the session's", got)
	}
}

// Saving is enough: the configuration is re-read without anything to ask, and saving without having
// changed anything stirs nothing.
func TestSavingReloads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	declare := func(target string) {
		t.Helper()
		body := `export default {
    port: 4300,
    ingress: [{ host: "localhost", routes: [{ path: "/", app: "app" }, { path: "/api", proxy: { target: "` + target + `" } }] }],
    apps: [{ name: "app", command: ["sh", "-c", "sleep 300 & wait", "{port}"] }],
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declare("https://first.example")
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.watch(ctx)

	declare("https://second.example")
	waitFor(t, "the configuration re-read on its own", func() bool {
		in, _ := session.Facade("localhost")
		route, _ := in.Match("/api/x")
		return route.Proxy.URL == "https://second.example"
	})

	// A save that changes nothing must neither stir the logs nor reload the browsers. It is
	// digested when touched falls back: the re-read happened, all that remains is to check it said
	// nothing.
	before := len(session.Log.Lines())
	declare("https://second.example")
	waitFor(t, "the save digested", func() bool { return !session.touched() })
	if after := len(session.Log.Lines()); after != before {
		t.Errorf("the log grew by %d lines for a save without change", after-before)
	}

	// A project directory is full of .ts files that are none of the tool's business: only its own
	// count.
	if err := os.WriteFile(filepath.Join(dir, "eslint.config.ts"), []byte("export default [];"), 0o644); err != nil {
		t.Fatal(err)
	}
	if session.touched() {
		t.Errorf("a file foreign to the %s suffix would make the configuration be re-read", config.Suffix)
	}
	// A neighbor that carries the suffix does count: that is where the targets one does not commit
	// live.
	if err := os.WriteFile(filepath.Join(dir, "proxies"+config.Suffix), []byte("export const api = {};"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !session.touched() {
		t.Errorf("a neighbor in %s should make the configuration be re-read", config.Suffix)
	}
}

// The kernel bell: a write in the directory rings at once. It is what makes the re-read immediate —
// the fallback poll, ten thousand times slower, only covers its failure.
func TestRingBellsOnWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bell, quiet := ring([]string{dir})
	defer quiet()
	if err := os.WriteFile(filepath.Join(dir, "neighbor"+config.Suffix), []byte("export {};"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bell:
	case <-time.After(2 * time.Second):
		t.Fatal("the bell did not ring for a write in the directory")
	}
}

// The command line's port is a session choice: the file cannot take it back, and a reload therefore
// has nothing to say about it — no "the port is taken" on every save.
func TestForcedPortStaysQuiet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	declare := func(target string) {
		t.Helper()
		body := `console.log("targets recomputed");
export default {
    port: 4200,
    ingress: [{ host: "localhost", routes: [{ path: "/", app: "app" }, { path: "/api", proxy: { target: "` + target + `" } }] }],
    apps: [{ name: "app", command: ["sh", "-c", "sleep 300 & wait", "{port}"] }],
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declare("https://first.example")
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port, cfg.ForcedPort = 4300, 4300
	session := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	if got := joinLines(session.Log.Lines()); !strings.Contains(got, "targets recomputed") {
		t.Errorf("what the script said at load should be readable in the log:\n%s", got)
	}

	declare("https://second.example")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := session.Config().Port; got != 4300 {
		t.Errorf("port = %d, want the command line's", got)
	}
	journal := joinLines(session.Log.Lines())
	if strings.Contains(journal, "listening on port") {
		t.Errorf("the port did not change: nothing to announce, yet the log says:\n%s", journal)
	}
	if strings.Count(journal, "targets recomputed") != 2 {
		t.Errorf("the script's echo should accompany the reload:\n%s", journal)
	}
	if session.Generation() != 1 {
		t.Errorf("generation = %d, want 1 after an applied reload", session.Generation())
	}

	// Re-reading without change: nothing to say, nothing to generate — even when the port is
	// forced.
	before := len(session.Log.Lines())
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if after := len(session.Log.Lines()); after != before {
		t.Errorf("the log grew by %d lines for a re-read without change", after-before)
	}
	if session.Generation() != 1 {
		t.Errorf("generation = %d, the re-read without change should not move it", session.Generation())
	}
}

// The file's port is followed as soon as the configuration applies: dsvs adapts to its
// configuration. A port that cannot be taken makes the configuration refused, which says so, and
// the session stays where it is. The command line's one wins: the file does not take it back.
func TestTheSessionFollowsThePortOfTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	declare := func(port string) {
		t.Helper()
		body := `export default {
    port: ` + port + `,
    ingress: [{ host: "localhost", routes: [{ path: "/", app: "app" }] }],
    apps: [{ name: "app", command: ["sh", "-c", "sleep 300 & wait", "{port}"] }],
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declare("4300")
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(form, *cfg, heldNet(t), ownPort{taken: 4500}, func(*Session) Gateway { return &spyGateway{} })

	declare("4400")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := session.Config().Port; got != 4400 {
		t.Errorf("port = %d, want the file's", got)
	}
	if journal := joinLines(session.Log.Lines()); !strings.Contains(journal, "-- listening on port 4400 instead of 4300") {
		t.Errorf("the port change must be said, yet the log carries:\n%s", journal)
	}

	declare("4500")
	refused := "port 4500: taken by another program; the session stays on port 4400"
	if err := session.Reload(); err == nil || err.Error() != refused {
		t.Errorf("reload = %v, want %q", err, refused)
	}
	if got, why := session.Config().Port, session.NotApplied(); got != 4400 || why != refused {
		t.Errorf("port = %d, not applied: %q; want 4400 and %q", got, why, refused)
	}

	forced := NewSession(form, *cfg, heldNet(t), ownPort{taken: 4400}, func(*Session) Gateway { return &spyGateway{} })
	forced.mu.Lock()
	forced.cfg.Port, forced.cfg.ForcedPort = 4600, 4600
	forced.mu.Unlock()
	declare("4400")
	if err := forced.Reload(); err != nil || forced.Config().Port != 4600 {
		t.Errorf("forced port = %d (%v), want 4600 whatever the file says", forced.Config().Port, err)
	}
}

// An unreadable configuration changes nothing: the session carries on with the running one, and the
// log carries the error.
func TestBadReloadChangesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	if err := os.WriteFile(path, []byte(`export default {
    port: 4300,
    ingress: [{ host: "localhost", routes: [{ path: "/", app: "app" }] }],
    apps: [{ name: "app", command: ["sh", "{port}"] }],
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })

	if err := os.WriteFile(path, []byte(`export default { port: 4300, ingress: [], apps: [] }`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := session.Reload(); err == nil {
		t.Fatal("a configuration with neither app nor facade was accepted")
	}
	if got := session.Facades(); len(got) != 1 {
		t.Errorf("facades = %v, want those from before the failed reload", names(got))
	}
	if got := joinLines(session.Log.Lines()); !strings.Contains(got, "no app declared") {
		t.Errorf("the session log should carry the error:\n%s", got)
	}
}

func names(facades []*Ingress) []string {
	out := make([]string, 0, len(facades))
	for _, in := range facades {
		out = append(out, in.Cfg.Name)
	}
	return out
}

func joinLines(lines []Line) string {
	var out strings.Builder
	for _, line := range lines {
		out.WriteString(line.Text + "\n")
	}
	return out.String()
}

// An app no ingress serves has no path to knock on: the open port is then all that can be asked.
func TestReadinessFallsBackToThePort(t *testing.T) {
	t.Parallel()
	if got := served(config.Config{}, "app"); got != "" {
		t.Errorf("probe path = %q, want none", got)
	}
	cfg := config.Config{Ingresses: []config.Ingress{config.NewIngress(config.Ingress{
		Host: "localhost",
		Routes: []config.Route{
			{Path: "/app/api", App: "app"},
			{Path: "/", App: "root"},
		},
	})}}
	if got := served(cfg, "app"); got != "/app/api/" {
		t.Errorf("probe path = %q, want the route's", got)
	}
	if got := served(cfg, "root"); got != "/" {
		t.Errorf("an app served at the root is asked there, returned %q", got)
	}
}

// An app is a process: a declaration that changes does not cut it from under whoever uses it. It
// says it runs with the previous one, and its restart takes the new one. An app the configuration
// no longer declares runs until it is stopped and then leaves, restarting in the meantime with what
// it had; a declared app appears and starts.
func TestAppsFollowTheDeclaration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	// Each declared app has its route: a facade leading to a departed app would be refused at load,
	// and rightly so — both are removed in the same gesture.
	declare := func(specs ...string) {
		t.Helper()
		var apps, routes []string
		for _, spec := range specs {
			name, mark, _ := strings.Cut(spec, ":")
			apps = append(apps,
				fmt.Sprintf(`{ name: %q, command: ["sh", "-c", "sleep 300 & wait", %q, "{port}"] }`, name, mark))
			routes = append(routes, fmt.Sprintf(`{ path: "/%s", app: %q }`, name, name))
		}
		body := fmt.Sprintf(`export default {
    port: 4300,
    ingress: [{ host: "localhost", routes: [%s] }],
    apps: [%s],
}`, strings.Join(routes, ", "), strings.Join(apps, ", "))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	declare("app:first")
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	supervising(t, session)
	app := session.App("app")
	app.Start()
	waitFor(t, "the app ready", func() bool { return app.Snapshot().Status == Ready })
	first := app.Snapshot().PID

	// A command that changes, and one more app.
	declare("app:second", "other:")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := app.Drift(); got != Stale {
		t.Errorf("drift = %v, want %v: the declaration changed while it runs", got, Stale)
	}
	if snap := app.Snapshot(); snap.PID != first || snap.Status != Ready {
		t.Errorf("the process moved on reload: %+v", snap)
	}
	newcomer := session.App("other")
	if newcomer == nil {
		t.Fatal("the app declared by the reload did not appear")
	}
	newcomer.Start()
	waitFor(t, "the newcomer ready", func() bool { return newcomer.Snapshot().Status == Ready })

	// The restart is what takes the new command.
	app.Restart()
	waitFor(t, "restarted", func() bool {
		snap := app.Snapshot()
		return snap.Status == Ready && snap.PID != first
	})
	if got := joinLines(app.Log.Lines()); !strings.Contains(got, "second") {
		t.Errorf("the restart did not take the declared command:\n%s", got)
	}
	if got := app.Drift(); got != Settled {
		t.Errorf("drift after restart = %v, want none", got)
	}

	// Removed from the configuration, it still runs — and restarts with what it had.
	declare("other:")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := app.Drift(); got != Dropped {
		t.Errorf("drift = %v, want %v: nothing declares it anymore", got, Dropped)
	}
	if session.App("app") != app {
		t.Fatal("a removed app that still runs should stay in the session")
	}
	before := app.Snapshot().PID
	app.Restart()
	waitFor(t, "the survivor restarted", func() bool {
		snap := app.Snapshot()
		return snap.Status == Ready && snap.PID != before
	})
	if got := app.Cfg().Command; len(got) < 4 || got[3] != "second" {
		t.Errorf("survivor's command = %v, want the one from before its removal", got)
	}

	// Put back in the configuration, with another command: all it needs is a restart.
	declare("app:first", "other:")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := app.Drift(); got != Stale {
		t.Errorf("drift = %v, want %v: the configuration declares it again", got, Stale)
	}

	// Removed for good: it leaves at the first stop.
	declare("other:")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	app.Stop()
	waitFor(t, "the survivor gone", func() bool { return session.App("app") == nil })
	if got := names(session.Facades()); len(got) != 1 {
		t.Errorf("facades = %v, want the configuration's", got)
	}
	if got := session.App("other").Snapshot().Status; got != Ready {
		t.Errorf("the app that stayed declared was disturbed: %v", got)
	}
}

// An app removed from the configuration without having run has nothing to finish: it leaves the
// session at once, and nothing says it still runs. It used to stay there forever, its supervisor
// waiting for a command without ever going back through the removal.
func TestAnIdleAppGoneFromTheConfigurationLeavesAtOnce(t *testing.T) {
	t.Parallel()
	session, declare := declaring(t, "front")

	declare("front", "docs")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if session.App("docs") == nil {
		t.Fatal("the app declared by the reload did not appear")
	}
	declare("front")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	// The departure is logged right after the removal: it is the line being waited for.
	waitFor(t, "docs partie", func() bool {
		return session.App("docs") == nil && strings.Contains(joinLines(session.Log.Lines()), "docs leaves")
	})
	if got := joinLines(session.Log.Lines()); strings.Contains(got, "docs still runs") {
		t.Errorf("the session log says docs still runs:\n%s", got)
	}
}

// A list of apps that was read no longer moves, even when an app leaves while it is being walked:
// the removal used to shift it in place, and whoever walked it — the browsers' reload, right after
// an app that never ran left — hit an empty slot and killed the session.
func TestTheAppsOnceReadStayAsTheyWere(t *testing.T) {
	t.Parallel()
	session, declare := declaring(t, "front", "docs")
	docs := session.App("docs")
	docs.Start()
	waitFor(t, "docs starts", func() bool { return docs.Snapshot().Status != Idle })
	declare("front")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}

	read := session.Apps()
	want := slices.Clone(read)
	docs.Stop()
	waitFor(t, "docs partie", func() bool { return session.App("docs") == nil })
	if !slices.Equal(read, want) {
		t.Errorf("the list read before docs left changed under its reader: %v, want %v", read, want)
	}
}

// Changing an app's color is no reason to restart it: the color does not reach the process.
func TestAColorIsNoReasonToRestart(t *testing.T) {
	t.Parallel()
	session, _ := declaring(t, "front", "search")
	search := session.App("search")
	search.Start()
	waitFor(t, "search starts", func() bool { return search.Snapshot().Status != Idle })

	path := session.Config().Path
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recolored := strings.Replace(string(body), `{ name: "search",`, `{ name: "search", color: "red",`, 1)
	if err := os.WriteFile(path, []byte(recolored), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := search.Cfg().Color; got != "red" {
		t.Fatalf("search's color is %q, want red: the test changes nothing", got)
	}
	if got := search.Drift(); got != Settled {
		t.Errorf("drift = %v, want none: only its color changed", got)
	}
	if got := joinLines(session.Log.Lines()); strings.Contains(got, "restart pending") {
		t.Errorf("the log announces a restart for a color:\n%s", got)
	}
}

// declaring sets up a supervised session on a configuration that declares these apps, each with its
// route, and returns how to rewrite the declaration: the reload is up to the test.
func declaring(t *testing.T, names ...string) (*Session, func(names ...string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), config.FileName)
	declare := func(names ...string) {
		t.Helper()
		var apps, routes []string
		for _, name := range names {
			apps = append(apps, fmt.Sprintf(`{ name: %q, command: ["sh", "-c", "sleep 300", "{port}"] }`, name))
			routes = append(routes, fmt.Sprintf(`{ path: "/%s", app: %q }`, name, name))
		}
		body := fmt.Sprintf(`export default {
    port: 4300,
    ingress: [{ host: "localhost", routes: [%s] }],
    apps: [%s],
}`, strings.Join(routes, ", "), strings.Join(apps, ", "))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declare(names...)
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	supervising(t, session)
	return session, declare
}

// supervising starts the session and waits for it to stop at the end of the test.
func supervising(t *testing.T, session *Session) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := session.Open(ctx); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		session.Run(ctx, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("the session did not stop")
		}
	})
}

// A JSON configuration does not carry the suffix of the tool's files, and a configuration that
// declares its own paths no longer watches its directory: in both cases, saving it must be enough.
// It is the configuration that is followed, not its name.
func TestSavingAConfigWithoutTheSuffixReloads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, config.JSONName)
	elsewhere := filepath.Join(t.TempDir(), "targets"+config.Suffix)
	if err := os.WriteFile(elsewhere, []byte("export const api = {};"), 0o644); err != nil {
		t.Fatal(err)
	}
	declare := func(target string) {
		t.Helper()
		body := fmt.Sprintf(`{
  "port": 4300,
  "watch": [%q],
  "ingress": [{ "host": "localhost", "routes": [
    { "path": "/", "app": "app" },
    { "path": "/api", "proxy": { "target": %q } }] }],
  "apps": [{ "name": "app", "command": ["sh", "-c", "sleep 300 & wait", "{port}"] }]
}`, elsewhere, target)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declare("https://first.example")
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.watch(ctx)

	declare("https://second.example")
	waitFor(t, "the configuration re-read on its own", func() bool {
		in, _ := session.Facade("localhost")
		route, _ := in.Match("/api/x")
		return route.Proxy.URL == "https://second.example"
	})
}

// paramsSession is a session whose configuration declares env, and whose proxy and app command
// depend on it: changing the value moves the one and makes the other stale. rewrite replaces the
// file, for the tests that change it under the session.
func paramsSession(t *testing.T) (session *Session, rewrite func(body string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), config.FileName)
	rewrite = func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rewrite(paramsBody(`const links = observed?.links.length ?? 0;`))
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session = NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	supervising(t, session)
	return session, rewrite
}

func paramsBody(prelude string) string {
	return `import { config, form } from "dsvs";
export default config(form({ env: ["local", "staging"] }), ({ env }, observed) => {
  ` + prelude + `
  return {
    port: 4300,
    ingress: [{ host: "localhost", routes: [
      { path: "/", app: "app" },
      { path: "/api", proxy: { target: "https://" + env + "-" + (typeof links === "undefined" ? 0 : links) + ".example" } },
    ] }],
    apps: [{ name: "app", command: ["sh", "-c", "sleep 300 & wait", env, "{port}"] }],
  };
});`
}

func apiTarget(t *testing.T, session *Session) string {
	t.Helper()
	in, _ := session.Facade("localhost")
	route, _ := in.Match("/api/x")
	return route.Proxy.URL
}

func TestSettingAValueMovesTheProxyFirst(t *testing.T) {
	t.Parallel()
	session, _ := paramsSession(t)
	before := session.Generation()

	if err := session.Set(map[string]string{"env": "staging"}, "cli"); err != nil {
		t.Fatal(err)
	}
	if got := apiTarget(t, session); got != "https://staging-0.example" {
		t.Errorf("target of /api = %q, want the staging one", got)
	}
	if got := session.Generation(); got != before+1 {
		t.Errorf("generation = %d, want %d", got, before+1)
	}
	if got := joinLines(session.Log.Lines()); !strings.Contains(got, "configuration reloaded") {
		t.Errorf("the session log does not tell the replay:\n%s", got)
	}
}

func TestAValueTheConfigurationDoesNotTakeChangesNothing(t *testing.T) {
	t.Parallel()
	session, _ := paramsSession(t)
	generation, cfg := session.Generation(), session.Config()

	err := session.Set(map[string]string{"env": "nope"}, "cli")
	if err == nil || !strings.Contains(err.Error(), "local, staging") {
		t.Fatalf("Set = %v, want a refusal that quotes the choices", err)
	}
	if session.Generation() != generation || !reflect.DeepEqual(session.Config(), cfg) {
		t.Error("a refused value changed the configuration in force")
	}
	if got := joinLines(session.Log.Lines()); !strings.Contains(got, "!! ") || !strings.Contains(got, "local, staging") {
		t.Errorf("the session log does not tell the refusal:\n%s", got)
	}
}

// A value whose built configuration is invalid is refused like any other: nothing changes, and the
// session does not lag behind its file for all that. It used to say "not applied", and the next
// valid setting wrote "configuration applied again".
func TestAValueThatBuildsAnInvalidConfigurationIsARefusal(t *testing.T) {
	t.Parallel()
	session, rewrite := paramsSession(t)
	rewrite(`import { config, form } from "dsvs";
export default config(form({ target: "https://staging.example" }), ({ target }) => ({
  port: 4300,
  ingress: [{ host: "localhost", routes: [
    { path: "/", app: "app" },
    { path: "/api", ...(target ? { proxy: { target } } : {}) },
  ] }],
  apps: [{ name: "app", command: ["sh", "-c", "sleep 300 & wait", "{port}"] }],
}));`)
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	generation := session.Generation()

	if err := session.Set(map[string]string{"target": ""}, "cli"); err == nil || !strings.Contains(err.Error(), `route "/api"`) {
		t.Fatalf("Set target= : %v, want the route's refusal", err)
	}
	if got := session.NotApplied(); got != "" {
		t.Errorf("not applied = %q after a refused gesture, want nothing: the file applies", got)
	}
	if session.Generation() != generation {
		t.Error("a refused gesture changed the configuration in force")
	}
	if err := session.Set(map[string]string{"target": "https://demo.example"}, "cli"); err != nil {
		t.Fatal(err)
	}
	if got := joinLines(session.Log.Lines()); strings.Contains(got, "configuration applied again") {
		t.Errorf("the log says the configuration applied again, though it never stopped being applied:\n%s", got)
	}
}

func TestAValueThatChangesAnAppLeavesItPending(t *testing.T) {
	t.Parallel()
	session, _ := paramsSession(t)
	app := session.App("app")
	app.Start()
	waitFor(t, "the app ready", func() bool { return app.Snapshot().Status == Ready })

	if err := session.Set(map[string]string{"env": "staging"}, "cli"); err != nil {
		t.Fatal(err)
	}
	if got := app.Drift(); got != Stale {
		t.Errorf("drift = %v, want %v: its command depends on the value", got, Stale)
	}
}

// A network change the configuration ignores reloads nothing, not even the browsers: the session
// records the observation and says so, one line, so that the pushed state carries it.
func TestAnObservationThatChangesNothingIsSilent(t *testing.T) {
	t.Parallel()
	session, rewrite := paramsSession(t)
	rewrite(paramsBody(``))
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	app := session.App("app")
	app.Start()
	waitFor(t, "the app ready", func() bool { return app.Snapshot().Status == Ready })
	generation, lines := session.Generation(), len(session.Log.Lines())

	seen := &config.Observed{Links: []config.Link{{Name: "tun0", Type: "tuntap", Addrs: []string{"10.8.0.2/24"}}}}
	if err := session.Observe(seen); err != nil {
		t.Fatal(err)
	}
	if session.Generation() != generation || len(session.Log.Lines()) != lines+1 {
		t.Errorf("an observation without effect reloaded the session:\n%s", joinLines(session.Log.Lines()))
	}
	if got := joinLines(session.Log.Lines()); !strings.Contains(got, "-- network: not observed → offline (tun0), configuration unchanged") {
		t.Errorf("the log does not tell the change:\n%s", got)
	}
	if got := joinLines(session.Log.Lines()); strings.Contains(got, "typed") || strings.Contains(got, "pages reload") {
		t.Error("the browsers were reloaded")
	}
	if session.Observed() != seen || session.Config().Observed != seen {
		t.Error("the session and its configuration must keep the last observation")
	}
}

// An applied configuration types into the terminal of a ready app exactly what its reload key
// declares, and nothing into that of an app that declares none.
func TestAnAppliedConfigurationTypesWhatAnAppDeclares(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), config.FileName)
	declare := func(target string) {
		t.Helper()
		body := `export default {
    port: 4300,
    ingress: [{ host: "localhost", routes: [{ path: "/api", proxy: { target: "` + target + `" } }] }],
    apps: [
        { name: "typist", command: ["sh", "-c", "read l; echo \"got [$l]\" | tr a-z A-Z; sleep 300"], reload: { stdin: "go\n" } },
        { name: "quiet", command: ["sh", "-c", "read l; echo \"got [$l]\" | tr a-z A-Z; sleep 300"] },
    ],
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declare("https://first.example")
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	supervising(t, session)
	for _, app := range session.Apps() {
		app.Start()
		waitFor(t, app.Name+" ready", app.IsReady)
	}

	declare("https://second.example")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	typist, quiet := session.App("typist"), session.App("quiet")
	waitFor(t, "what typist read", func() bool { return strings.Contains(joinLines(typist.Log.Lines()), "GOT [GO]") })
	if got := joinLines(session.Log.Lines()); !strings.Contains(got, `-- typist: "go\n" typed`) || strings.Contains(got, "quiet:") {
		t.Errorf("the log must say what was typed, and only for typist:\n%s", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := joinLines(quiet.Log.Lines()); strings.Contains(got, "GOT") {
		t.Errorf("quiet declared nothing, and read: %s", got)
	}
}

func exitsVia(gateway string) *config.Observed {
	return &config.Observed{Links: []config.Link{{Name: "eth0", Type: "dummy", Addrs: []string{"192.0.2.17/32"}}},
		Exits: []config.Exit{{Via: gateway, Dev: "eth0"}}}
}

func networkLines(session *Session) []string {
	var told []string
	for _, line := range session.Log.Lines() {
		if strings.Contains(line.Text, "-- network:") {
			told = append(told, line.Text)
		}
	}
	return told
}

// Same interface, same address, another gateway: a single replay, the proxy's target changes, and
// the change is said once.
func TestANetworkChangeIsToldOnce(t *testing.T) {
	t.Parallel()
	session, rewrite := paramsSession(t)
	rewrite(paramsBody(`const links = (observed?.exits[0]?.via ?? "none").replaceAll(".", "-");`))
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct{ gateway, target, told string }{
		{"198.51.100.1", "https://local-198-51-100-1.example", "-- network: not observed → eth0 via 198.51.100.1, configuration replayed"},
		{"198.51.100.2", "https://local-198-51-100-2.example", "-- network: eth0 via 198.51.100.1 → eth0 via 198.51.100.2, configuration replayed"},
	} {
		generation, told := session.Generation(), len(networkLines(session))
		if err := session.Observe(exitsVia(step.gateway)); err != nil {
			t.Fatal(err)
		}
		if got := apiTarget(t, session); got != step.target {
			t.Errorf("target of /api = %q, want %q", got, step.target)
		}
		if got := session.Generation(); got != generation+1 {
			t.Errorf("generation = %d, want %d", got, generation+1)
		}
		if lines := networkLines(session); len(lines) != told+1 || lines[len(lines)-1] != step.told {
			t.Errorf("network lines %q, want one more: %q", lines, step.told)
		}
	}

	again := len(networkLines(session))
	if err := session.Observe(exitsVia("198.51.100.2")); err != nil {
		t.Fatal(err)
	}
	if got := len(networkLines(session)); got != again {
		t.Errorf("the same observation was said once more: %q", networkLines(session))
	}
}

// When the configuration does not depend on the network, the change is said anyway, and an event
// goes out: it is what makes the panel re-read the state.
func TestANetworkChangeTheConfigurationIgnoresStillReachesTheState(t *testing.T) {
	t.Parallel()
	session, rewrite := paramsSession(t)
	rewrite(paramsBody(``))
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	events, stop := session.Watch()
	defer stop()
	generation := session.Generation()

	if err := session.Observe(exitsVia("198.51.100.2")); err != nil {
		t.Fatal(err)
	}
	if got := session.Generation(); got != generation {
		t.Errorf("generation = %d, want %d: nothing depends on it", got, generation)
	}
	if lines := networkLines(session); len(lines) != 1 || !strings.HasSuffix(lines[0], "configuration unchanged") {
		t.Errorf("network lines %q, want \"configuration unchanged\"", lines)
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Error("no event: the state would not be pushed")
	}
}

func TestAnObservationOutlivesAFailedReplay(t *testing.T) {
	t.Parallel()
	session, rewrite := paramsSession(t)
	rewrite(paramsBody(`if (observed) throw new Error("boom");`))
	seen := &config.Observed{Links: []config.Link{{Name: "tun0", Type: "tuntap", Addrs: []string{"10.8.0.2/24"}}}}

	if err := session.Observe(seen); err == nil {
		t.Fatal("the configuration that fails on this observation should say so")
	}
	if session.Observed() != seen {
		t.Fatal("the observation is a fact: it stays even when the replay fails")
	}

	rewrite(paramsBody(`const links = observed.links.length;`))
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := apiTarget(t, session); got != "https://local-1.example" {
		t.Errorf("target of /api = %q, want a configuration built on the new observation", got)
	}
}

// A configuration the file builds but validation refuses has no line: its refusal names the file,
// and all its faults fit on the line the status shows.
func TestARefusalOfSeveralFaultsReadsOnOneLine(t *testing.T) {
	t.Parallel()
	session, rewrite := paramsSession(t)
	rewrite(`export default { port: 4300, ingress: [{ host: "localhost", routes: [{ path: "/a", app: "a" }, { path: "/b", app: "b" }] }],
  apps: [{ name: "a", command: ["srv"] }, { name: "b", command: ["srv"] }] };`)
	if err := session.Reload(); err == nil {
		t.Fatal("an invalid configuration reloads")
	}
	got := session.NotApplied()
	if !strings.HasPrefix(got, "config.dsvs.ts: ") || strings.Count(got, "{port}") != 2 || strings.Contains(got, "\n") {
		t.Errorf("not applied = %q, want the file and its two faults on one line", got)
	}
}

// A configuration refused without a gesture — a save that breaks it, a network where it fails —
// keeps the one in force and says why, until the evaluation that applies it again. That one is
// logged even when it changes nothing: going back to the previous content, or to the network the
// configuration in force comes from.
func TestARefusedConfigurationSaysWhyUntilItAppliesAgain(t *testing.T) {
	t.Parallel()
	session, rewrite := paramsSession(t)
	valid := paramsBody(`if (observed?.exits[0]?.via === "203.0.113.9") throw new Error("unknown gateway");`)
	rewrite(valid + ",")
	generation := session.Generation()
	if err := session.Reload(); err == nil {
		t.Fatal("an unreadable configuration reloads")
	}
	if got := session.NotApplied(); !regexp.MustCompile(`^config\.dsvs\.ts:\d+:\d+: SyntaxError[^\n]+$`).MatchString(got) {
		t.Errorf("not applied = %q, want the syntax error and its location, on one line", got)
	}
	if got := apiTarget(t, session); got != "https://local-0.example" {
		t.Errorf("target of /api = %q, want the one of the configuration in force", got)
	}

	events, stop := session.Watch()
	defer stop()
	lastLine := func() string {
		lines := strings.Split(strings.TrimSpace(joinLines(session.Log.Lines())), "\n")
		return lines[len(lines)-1]
	}
	rewrite(valid)
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := session.NotApplied(); got != "" || session.Generation() != generation {
		t.Errorf("back to the content in force: not applied = %q, generation +%d", got, session.Generation()-generation)
	}
	if got := lastLine(); got != "-- configuration applied again" {
		t.Errorf("the log ends on %q", got)
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Error("no event: the state would not be pushed")
	}

	home := exitsVia("198.51.100.1")
	if err := session.Observe(home); err != nil {
		t.Fatal(err)
	}
	if err := session.Observe(exitsVia("203.0.113.9")); err == nil {
		t.Fatal("the configuration that throws on this gateway should say so")
	}
	if got := session.NotApplied(); !regexp.MustCompile(`^config\.dsvs\.ts:\d+:\d+: Error: unknown gateway$`).MatchString(got) {
		t.Errorf("not applied = %q, want the thrown error and its location", got)
	}
	if err := session.Observe(home); err != nil {
		t.Fatal(err)
	}
	if got := session.NotApplied(); got != "" {
		t.Errorf("back on the network of the configuration in force, not applied = %q", got)
	}
}

// The form scenarios, in design order, on the proxy's target, the generation and the exact log
// lines: what a gesture keeps and where, choices that reshape with the network, a refusal that
// changes nothing, a new session that starts again from what was kept, and a form the context makes
// invalid, which keeps the configuration in force until the gesture that fixes it.
func TestTheFormFollowsTheNetworkAndItsRules(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), config.FileName)
	if err := os.WriteFile(path, []byte(formBody), 0o644); err != nil {
		t.Fatal(err)
	}
	tunnel := config.Link{Name: "tun0", Type: "tuntap", Addrs: []string{"10.8.0.2/24"}}
	a, b, c := exitsVia("198.51.100.1"), exitsVia("198.51.100.2"), exitsVia("203.0.113.9")
	a.Links, c.Links = append(a.Links, tunnel), append(c.Links, tunnel)
	offline := &config.Observed{Links: a.Links}
	opened := func(seen *config.Observed) *Session {
		t.Helper()
		form, cfg, err := config.Load(path, nil, InputFor(path, seen, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		session := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
		supervising(t, session)
		return session
	}
	session := opened(a)

	told := func(want ...string) {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(joinLines(session.Log.Lines())), "\n")
		got := lines[max(0, len(lines)-len(want)):]
		if !slices.Equal(got, want) {
			t.Errorf("the log ends on %q, want %q", got, want)
		}
	}
	after := func(replays uint64, target string, gesture func() error) error {
		t.Helper()
		generation := session.Generation()
		err := gesture()
		if got := session.Generation(); got != generation+replays {
			t.Errorf("generation +%d, want +%d", got-generation, replays)
		}
		if got := apiTarget(t, session); got != target {
			t.Errorf("/api leads to %q, want %q", got, target)
		}
		return err
	}
	set := func(values map[string]string) func() error { return func() error { return session.Set(values, "cli") } }
	move := func(seen *config.Observed) func() error { return func() error { return session.Observe(seen) } }
	from := func(name, value, origin string) {
		t.Helper()
		field, _ := session.Form().Field(name)
		if field.Value != value || field.From != origin {
			t.Errorf("%s = %q from %q, want %q from %q", name, field.Value, field.From, value, origin)
		}
	}

	// 2. The pair goes through together, and each value is kept where the form says.
	if err := after(1, "https://staging-vpn-en.example", set(map[string]string{"env": "staging", "api.staging": "vpn"})); err != nil {
		t.Fatal(err)
	}
	told("-- env=staging kept", "-- api.staging=vpn kept for eth0 via 198.51.100.1")

	// 3. On B, unknown, vpn is not among the choices: the default answers, and the intent comes
	//    back on A.
	if err := after(1, "https://staging-office-en.example", move(b)); err != nil {
		t.Fatal(err)
	}
	told("-- network: eth0 via 198.51.100.1, tun0 → eth0 via 198.51.100.2, configuration replayed, now api.staging=office")
	from("api.staging", "office", "default")
	if err := after(1, "https://staging-vpn-en.example", move(a)); err != nil {
		t.Fatal(err)
	}
	told("-- network: eth0 via 198.51.100.2 → eth0 via 198.51.100.1, tun0, configuration replayed, now api.staging=vpn")
	from("api.staging", "vpn", "network")

	// An unknown network where the intent holds changes nothing; offline, nothing is kept for a
	// network.
	if err := after(0, "https://staging-vpn-en.example", move(c)); err != nil {
		t.Fatal(err)
	}
	told("-- network: eth0 via 198.51.100.1, tun0 → eth0 via 203.0.113.9, tun0, configuration unchanged")
	if err := after(0, "https://staging-vpn-en.example", move(offline)); err != nil {
		t.Fatal(err)
	}
	if err := after(1, "https://staging-public-en.example", set(map[string]string{"api.staging": "public"})); err != nil {
		t.Fatal(err)
	}
	told("-- api.staging=public not kept: offline")
	if err := after(1, "https://staging-vpn-en.example", move(a)); err != nil {
		t.Fatal(err)
	}
	told("-- network: offline (eth0, tun0) → eth0 via 198.51.100.1, tun0, configuration replayed, now api.staging=vpn")

	// 6. On B, it is vpn, in force on A, that stays, and not public chosen offline before it:
	//    outside B's choices, it yields to the default there. With public, fr is refused, and
	//    nothing changes.
	if err := after(1, "https://staging-office-en.example", move(b)); err != nil {
		t.Fatal(err)
	}
	if err := after(1, "https://staging-public-en.example", set(map[string]string{"api.staging": "public"})); err != nil {
		t.Fatal(err)
	}
	told("-- api.staging=public kept for eth0 via 198.51.100.2")
	shown := session.Form()
	err := after(0, "https://staging-public-en.example", set(map[string]string{"locale": "fr"}))
	if err == nil || err.Error() != "locale: public has no fr build" {
		t.Errorf("set locale=fr = %v, want the form's refusal", err)
	}
	if !reflect.DeepEqual(session.Form(), shown) {
		t.Error("a refused gesture changed the form")
	}

	// 7. A new session on B starts again from what the project and this network keep.
	session = opened(b)
	from("env", "staging", "kept")
	from("api.staging", "public", "network")
	from("delay", "0", "default")
	if got := apiTarget(t, session); got != "https://staging-public-en.example" {
		t.Errorf("the new session leads /api to %q", got)
	}

	// 8. On A, vpn and fr go together; on B, fr no longer holds with public: it yields to en, the
	//    configuration applies, and the log says so. Back on A, fr returns: nothing erased it.
	if err := after(1, "https://staging-vpn-en.example", move(a)); err != nil {
		t.Fatal(err)
	}
	if err := after(1, "https://staging-vpn-fr.example", set(map[string]string{"locale": "fr"})); err != nil {
		t.Fatal(err)
	}
	told("-- locale=fr kept")
	if err := after(1, "https://staging-public-en.example", move(b)); err != nil {
		t.Fatal(err)
	}
	told("-- locale=fr no longer holds (public has no fr build): locale=en until it does",
		"-- configuration reloaded: 1 app, 1 facade, 0 groups",
		"-- network: eth0 via 198.51.100.1, tun0 → eth0 via 198.51.100.2, configuration replayed, now api.staging=public locale=en")
	from("locale", "en", "default")
	if got := session.NotApplied(); got != "" {
		t.Errorf("not applied = %q, want an applied configuration", got)
	}
	if err := after(1, "https://staging-vpn-fr.example", move(a)); err != nil {
		t.Fatal(err)
	}
	from("locale", "fr", "session")
	if got := joinLines(session.Log.Lines()); strings.Contains(got, "!! ") {
		t.Errorf("the log carries an error:\n%s", got)
	}
}

// On a network that kept nothing for api, the value in force stays: neither the last choice made
// elsewhere, nor what the project keeps, nor the default. A new session has nothing in force: the
// project, then the default. An option disabled on this network yields to the default there, like
// any intent.
func TestAnUnseenNetworkKeepsTheValueInForce(t *testing.T) {
	office, home, vpn, hotel := exitsVia("198.51.100.1"), exitsVia("203.0.113.9"), exitsVia("198.51.100.2"), exitsVia("203.0.113.50")
	for _, c := range []struct{ rules, opened string }{{"network(p.api);", "local"}, {"keep(p.api); network(p.api);", "demo"}} {
		t.Run(c.rules, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), config.FileName)
			if err := os.WriteFile(path, []byte(strings.Replace(unseenBody, "RULES", c.rules, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			var session *Session
			open := func(seen *config.Observed) {
				t.Helper()
				form, cfg, err := config.Load(path, nil, InputFor(path, seen, nil, nil))
				if err != nil {
					t.Fatal(err)
				}
				session = NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
				supervising(t, session)
			}
			do := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			api := func(value, origin string) {
				t.Helper()
				field, _ := session.Form().Field("api")
				if field.Value != value || field.From != origin {
					t.Errorf("api = %q from %q, want %q from %q", field.Value, field.From, value, origin)
				}
			}

			open(office)
			do(session.Set(map[string]string{"api": "staging"}, "cli"))
			do(session.Observe(home))
			do(session.Set(map[string]string{"api": "demo"}, "cli"))
			do(session.Observe(office))
			api("staging", "network")
			do(session.Observe(vpn))
			api("staging", "session")
			if got := apiTarget(t, session); got != "https://staging.example" {
				t.Errorf("/api leads to %q, want staging", got)
			}
			do(session.Observe(hotel))
			api("local", "default")

			// Reopened at the office, the session has staging in force there, and the VPN keeps it.
			open(office)
			api("staging", "network")
			do(session.Observe(vpn))
			api("staging", "session")

			open(vpn)
			api(c.opened, map[string]string{"local": "default", "demo": "kept"}[c.opened])
		})
	}
}

// unseenBody is the shop whose api is chosen per network; staging cannot be reached from the hotel.
const unseenBody = `import { choices, config, form, keep, network } from "dsvs";
const params = form({ api: ["local", "staging", "demo"] }, (p, observed) => {
  RULES
  const hotel = observed?.exits.some((exit) => exit.via === "203.0.113.50") ?? false;
  choices(p.api, ["local", { value: "staging", disabled: hotel && "staging is not reachable from here" }, "demo"]);
});
export default config(params, ({ api }) => ({
  port: 4300,
  ingress: [{ host: "localhost", routes: [{ path: "/api", proxy: { target: "https://" + api + ".example" } }] }],
  apps: [{ name: "app", command: ["sh", "-c", "sleep 300 & wait", "{port}"] }],
}));`

// formBody is the scenarios' form: env and locale kept for the project, an api group that follows
// the network and does not exist locally, whose choices depend on a tunnel, a delay that only
// exists locally, and a refused pair.
const formBody = `import { choices, config, defaultValue, disabled, form, hidden, keep, max, min, network, validateTree } from "dsvs";
const params = form({ env: ["local", "staging"], api: { staging: ["office", "vpn", "public"], mirror: false }, delay: 0, locale: ["en", "fr"] }, (p, observed) => {
  keep(p.env);
  keep(p.locale);
  hidden(p.api, { when: ({ valueOf }) => valueOf(p.env) === "local" });
  network(p.api);
  const tunnel = observed?.links.some((link) => link.name === "tun0") ?? false;
  choices(p.api.staging, tunnel ? ["office", "vpn", "public"] : ["office", "public"]);
  defaultValue(p.api.staging, tunnel ? "vpn" : "office");
  disabled(p.api.mirror, { when: !tunnel && "the mirror is only reachable through the vpn" });
  hidden(p.delay, { when: ({ valueOf }) => valueOf(p.env) !== "local" });
  min(p.delay, 0);
  max(p.delay, 5000);
  validateTree(p, ({ valueOf }) => valueOf(p.locale) === "fr" && valueOf(p.api.staging) === "public" ? { field: p.locale, message: "public has no fr build" } : null);
});
export default config(params, ({ env, api, delay, locale }) => ({
  port: 4300,
  ingress: [{ host: "localhost", routes: [
    { path: "/", app: "app" },
    { path: "/api", proxy: { target: "https://" + (env === "local" ? "local" : "staging-" + api.staging) + "-" + locale + ".example" } },
  ] }],
  apps: [{ name: "app", command: ["sh", "-c", "sleep 300 & wait", String(delay), "{port}"] }],
}));`

// A draft fills in in whatever order one likes: staging, which requires an account, stays chosen as
// long as the account is missing, nothing applies, and the typed account applies both together —
// only then are they kept. A submission, on the other hand, refuses what leaves the form invalid,
// and a draft refuses what a field does not take.
func TestADraftWaitsForTheFieldThatCompletesIt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), config.FileName)
	if err := os.WriteFile(path, []byte(draftBody), 0o644); err != nil {
		t.Fatal(err)
	}
	office := exitsVia("198.51.100.1")
	form, cfg, err := config.Load(path, nil, InputFor(path, office, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	supervising(t, session)

	shown := session.Form()
	if err := session.Set(map[string]string{"api": "staging"}, "cli"); err == nil || err.Error() != "the form refuses it: client: staging needs a test account" {
		t.Errorf("set api=staging = %v, want the form's refusal", err)
	}
	if !reflect.DeepEqual(session.Form(), shown) {
		t.Error("a refused submission changed the form")
	}
	if err := session.Draft(map[string]string{"api": "nope"}, "panel"); err == nil || err.Error() != `api: "nope" is not one of local, staging` {
		t.Errorf("draft api=nope = %v, want the field's refusal", err)
	}

	if err := session.Draft(map[string]string{"api": "staging"}, "panel"); err != nil {
		t.Fatal(err)
	}
	if api, client := session.Form().Fields[0], session.Form().Fields[1]; api.Value != "staging" || !slices.Equal(client.Errors, []string{"staging needs a test account"}) {
		t.Errorf("api = %q, client says %q; want staging, and the missing account", api.Value, client.Errors)
	}
	if got := apiTarget(t, session); got != "https://local.example" || session.NotApplied() != "client: staging needs a test account" {
		t.Errorf("/api leads to %q, not applied = %q: nothing should have applied", got, session.NotApplied())
	}
	if kept, onNetwork := projects.Recall(path, office.Network()); len(kept)+len(onNetwork) > 0 {
		t.Errorf("the draft kept %v and %v before applying", kept, onNetwork)
	}

	if err := session.Draft(map[string]string{"client": "lea@shop.test"}, "tui"); err != nil {
		t.Fatal(err)
	}
	if got := apiTarget(t, session); got != "https://staging.example" || session.NotApplied() != "" {
		t.Errorf("/api leads to %q, not applied = %q: the account should have applied everything", got, session.NotApplied())
	}
	kept, onNetwork := projects.Recall(path, office.Network())
	if !reflect.DeepEqual(kept, map[string]string{"client": "lea@shop.test"}) || !reflect.DeepEqual(onNetwork, map[string]string{"api": "staging"}) {
		t.Errorf("kept %v for the project and %v for the network, want the account and staging", kept, onNetwork)
	}
	lines := strings.Split(strings.TrimSpace(joinLines(session.Log.Lines())), "\n")
	if got, want := lines[len(lines)-2:], []string{"-- client=lea@shop.test kept", "-- api=staging kept for eth0 via 198.51.100.1"}; !slices.Equal(got, want) {
		t.Errorf("the log ends on %q, want %q", got, want)
	}
	// Each setting taken says where it comes from, the waiting one as well as the completing one.
	text := joinLines(session.Log.Lines())
	for _, want := range []string{"-- set api=staging (panel)\n-- configuration not applied", "-- set client=lea@shop.test (tui)", "-- configuration applied again"} {
		if !strings.Contains(text, want) {
			t.Errorf("the log does not say %q:\n%s", want, text)
		}
	}
}

// draftBody is the shop where staging requires a test account, kept for the project; the server
// follows the network.
const draftBody = `import { config, form, keep, network, required } from "dsvs";
const params = form({ api: ["local", "staging"], client: "" }, (p) => {
  network(p.api);
  keep(p.client);
  required(p.client, { when: ({ valueOf }) => valueOf(p.api) === "staging", message: "staging needs a test account" });
});
export default config(params, ({ api }) => ({
  port: 4300,
  ingress: [{ host: "localhost", routes: [{ path: "/", app: "app" }, { path: "/api", proxy: { target: "https://" + api + ".example" } }] }],
  apps: [{ name: "app", command: ["sh", "-c", "sleep 300 & wait", "{port}"] }],
}));`
