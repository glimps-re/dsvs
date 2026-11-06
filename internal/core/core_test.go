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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"dsvs/internal/config"
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
	case !last.ByUser:
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
	session := NewSession(cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy })
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
	session := NewSession(cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy })

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
	if last.ByUser || last.Code != 3 {
		t.Errorf("outcome = %+v, want an unrequested exit with code 3", last)
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
	session := NewSession(cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy })

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

// ownPort takes the port itself: a test has no hub to launch, and what it checks does not depend on
// who holds the port.
type ownPort struct{}

func (ownPort) Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", port))
}

func (ownPort) Hosts([]string) error { return nil }

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
	if session.Backend("unknown") != nil {
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
	session := NewSession(cfg, network, ownPort{}, func(*Session) Gateway { return &spyGateway{} })
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
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
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
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port, cfg.ForcedPort = 4300, 4300
	session := NewSession(cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
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
	if strings.Contains(journal, "is taken") {
		t.Errorf("the file's port did not change: nothing to announce, yet the log says:\n%s", journal)
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

// Without -port, a port changed in the file cannot be followed: the session keeps its own and says so
// — once, when it changes, not at every save as long as the gap persists.
func TestChangedPortIsKeptAndSaidOnce(t *testing.T) {
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
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })

	declare("4400")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := session.Config().Port; got != 4300 {
		t.Errorf("port = %d, want the session's", got)
	}
	journal := joinLines(session.Log.Lines())
	if !strings.Contains(journal, "port 4300 is taken") {
		t.Errorf("an ignored port must be said, yet the log carries:\n%s", journal)
	}

	before := len(session.Log.Lines())
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	if after := len(session.Log.Lines()); after != before {
		t.Errorf("the gap persists without changing: the log has no reason to say it again (%d more lines)", after-before)
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
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })

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
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
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
