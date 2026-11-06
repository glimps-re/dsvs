package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"

	"dsvs/internal/config"
)

const (
	logSize     = 4096
	eventBuffer = 256
	gracePeriod = 5 * time.Second
	probeEvery  = 200 * time.Millisecond

	// The configuration file is watched by the kernel (inotify): this poll is only the safety net
	// in case the bell breaks — it decides nothing, touched has the last word.
	watchFallback = 10 * time.Second

	// What an editor is given to finish its save burst — temporary file, write, rename — before
	// comparing: a read in the middle would see a half-written file.
	settleDelay = 50 * time.Millisecond

	// What the terminal reader is given to finish draining its buffer after the process dies. The
	// buffer is already readable by then: this delay only lets the read go through.
	drainGrace = 200 * time.Millisecond

	// A dev-server holds the request while it compiles: it is not awaited forever, it will be asked
	// again.
	probeTimeout = 3 * time.Second

	// Size of the terminal lent to the apps before the interface tells its own. Tools use it to
	// format their tables.
	defaultCols = 120
	defaultRows = 40
)

type Status int

const (
	Idle Status = iota
	Starting
	Ready
	Stopping
	Restarting
)

func (s Status) String() string {
	switch s {
	case Starting:
		return "starting"
	case Ready:
		return "ready"
	case Stopping:
		return "stopping"
	case Restarting:
		return "restarting"
	}
	return "idle"
}

// Drift says what separates an app's process from what the configuration declares now: it is what a
// reload cannot apply on its own, because an app is a process and it is not cut from under whoever
// uses it.
type Drift int

const (
	Settled Drift = iota
	// Stale: the declaration has changed since the start; a restart takes the new one.
	Stale
	// Dropped: the configuration no longer declares this app. It runs as long as it is left
	// running, restarts with the declaration it had, and disappears as soon as it stops.
	Dropped
)

func (d Drift) String() string {
	switch d {
	case Stale:
		return "restart pending"
	case Dropped:
		return "gone from the configuration"
	}
	return ""
}

// Outcome describes how the last run ended.
type Outcome struct {
	Code   int
	Signal string
	ByUser bool
	At     time.Time
}

func (o Outcome) String() string {
	switch {
	case o.ByUser:
		return "stopped"
	case o.Signal != "":
		return "killed (" + o.Signal + ")"
	default:
		return fmt.Sprintf("exited (%d)", o.Code)
	}
}

// Snapshot is the state of an app at a given moment. PID, Since and ReadyAt only hold in the
// statuses that carry them; Last describes the previous run when the app is Idle.
type Snapshot struct {
	Status   Status
	PID      int
	Since    time.Time
	ReadyAt  time.Time
	Last     *Outcome
	Restarts int
}

type EventKind int

const (
	Logged EventKind = iota
	Changed
	// Reloaded says the configuration has been replaced: the interfaces align with what it declares
	// now, facades included.
	Reloaded
)

type Event struct {
	Source string
	Kind   EventKind
}

// Gateway holds the session's port and dispatches to the facades it declares, like the ingress
// controller in production. The Go proxy is one implementation; nginx or another process would be
// another. Start and Stop bracket the session, once each: the port is taken before the interface
// and released on exit.
type Gateway interface {
	Start(context.Context) error
	Stop(context.Context) error
	Stats(ingress string) (requests, failures int)
}

// Endpoint is an app's address for the duration of a run: the network hands it out at start, and it
// dies with the run.
type Endpoint struct {
	IP   string
	Port int
}

func (e Endpoint) String() string { return net.JoinHostPort(e.IP, strconv.Itoa(e.Port)) }

// Network decides where the apps listen and how they are reached. Loopback is the default
// implementation; the virtual network is another, without anything else changing. The session's
// port is none of its business: it is Front's.
type Network interface {
	// Assign gives the app the address of its next run.
	Assign(app string) (Endpoint, error)
	// Dial opens a connection to that address, from where the proxy lives.
	Dial(ctx context.Context, ep Endpoint) (net.Conn, error)
	// Mode says which network was obtained, and why: the TUI turns it into a badge.
	Mode() Mode
}

// Front holds the port for the session, which never takes it itself, and routes back to it the
// connections of the hosts it declares. The hub is its implementation.
type Front interface {
	// Listen returns the connections of the session's port.
	Listen(port int) (net.Listener, error)
	// Hosts tells the hosts that must come back to us, the whole list every time — it is a
	// declaration, not a difference to apply.
	Hosts(hosts []string) error
}

// Mode names the network in use. Detail describes it in one line for the cards; Reason is only set
// when nothing better could be done, and Isolated says whether the apps are out of the host's
// reach.
type Mode struct {
	Name     string
	Detail   string
	Reason   string
	Isolated bool
}

// Loopback makes the apps listen on 127.0.0.1, on ports nobody configures. No isolation: it is the
// host's network. Reason says what prevented doing better, when it is a fallback and not a choice.
type Loopback struct{ Reason string }

func (Loopback) Assign(string) (Endpoint, error) {
	port, err := FreePort("127.0.0.1")
	return Endpoint{IP: "127.0.0.1", Port: port}, err
}

func (Loopback) Dial(ctx context.Context, ep Endpoint) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", ep.String())
}

func (l Loopback) Mode() Mode {
	return Mode{Name: "loopback", Detail: "127.0.0.1, ephemeral ports", Reason: l.Reason}
}

// FreePort takes a port only to release it at once: the app takes it back within the second. The
// race is possible but narrow, and a bind failure shows in the app's log.
func FreePort(ip string) (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Backend is all the proxy knows of an app: how to open a connection to it. Not its port, not even
// the fact that it has one.
type Backend interface {
	Name() string
	// Addr tells two runs of the same app apart, so that the proxy does not keep warm connections
	// to the previous address.
	Addr() string
	DialContext(ctx context.Context) (net.Conn, error)
}

type backend struct {
	name string
	ep   Endpoint
	net  Network
}

func (b *backend) Name() string { return b.name }
func (b *backend) Addr() string { return b.ep.String() }

func (b *backend) DialContext(ctx context.Context) (net.Conn, error) {
	return b.net.Dial(ctx, b.ep)
}

type App struct {
	// Name is the app's identity: it is by its name that a re-read configuration finds it again,
	// and it never changes — another name is another app.
	Name string
	Log  *Journal

	// declared returns what the configuration in force says about this app, and whether it still
	// says it: an app survives its removal as long as its process.
	declared func() (config.App, bool)

	// dismiss removes the app from the session, when nothing declares it anymore and its process is
	// gone. Returns false if it still has its place, and the supervisor carries on.
	dismiss func() bool

	// serves returns the path through which the app is asked whether it is ready, empty when none is
	// known: an open port is then all that can be checked. It is a question, not a value — the routes
	// that answer it may have been reloaded since the previous run.
	serves func() string

	net    Network
	cmds   chan command
	notify func(Event)

	// live carries the run's backend as soon as the app answers, and nothing otherwise: the proxy
	// has no other question to ask, and a stopped app has no representable address.
	live atomic.Pointer[backend]

	// mu guards the published state, the current run and the size lent to its terminal.
	mu   sync.RWMutex
	snap Snapshot
	// run is the declaration the last run was launched with: it is what the configuration's current
	// word is compared against, and it serves as the fallback for an app the configuration no
	// longer declares.
	run config.App
	// cur is the current run, nil when the app is stopped: its process, its terminal and its
	// address.
	cur  *run
	cols int
	rows int
}

// run is an app's process for the duration of one supervision round.
type run struct {
	proc *os.Process
	tty  *os.File
	ep   Endpoint
}

type command int

const (
	cmdStart command = iota
	cmdStop
	cmdRestart
)

func (s *App) Start()   { s.send(cmdStart) }
func (s *App) Stop()    { s.send(cmdStop) }
func (s *App) Restart() { s.send(cmdRestart) }

// Cfg returns the declaration that holds for the next start: that of the configuration in force, or
// that of the last run when the configuration no longer declares this app.
func (s *App) Cfg() config.App {
	if cfg, live := s.declared(); live {
		return cfg
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.run
}

// Drift says what separates the running process from the current declaration. A stopped app has
// nothing to catch up on: its next start takes what the configuration says then, and an app nothing
// declares anymore ceases to exist as soon as it stops.
func (s *App) Drift() Drift {
	s.mu.RLock()
	run, running := s.run, s.snap.Status != Idle
	s.mu.RUnlock()
	if !running {
		return Settled
	}
	cfg, live := s.declared()
	switch {
	case !live:
		return Dropped
	case !reflect.DeepEqual(run, cfg):
		return Stale
	}
	return Settled
}

func (s *App) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

// IsReady says whether the app accepts connections; it is what an ingress checks before routing.
func (s *App) IsReady() bool { return s.live.Load() != nil }

// Backend returns how to reach the app, or nil as long as it does not answer.
func (s *App) Backend() Backend {
	if b := s.live.Load(); b != nil {
		return b
	}
	return nil
}

// Send writes to the app's input, as if typing in its terminal.
func (s *App) Send(input string) error {
	s.mu.RLock()
	cur := s.cur
	s.mu.RUnlock()
	if cur == nil {
		return errors.New("the app is not running")
	}
	quietControlEcho(cur.tty)
	_, err := cur.tty.WriteString(input)
	return err
}

// Resize tells the app the size of the terminal lent to it; it also holds for its next starts.
func (s *App) Resize(cols, rows int) {
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	cur := s.cur
	s.mu.Unlock()
	s.Log.SetHeight(rows)
	if cur != nil {
		_ = pty.Setsize(cur.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
}

// Ingress is a facade of the session: a host and its routes. It is not a process and it does not
// start — the port is held for the whole session, and every facade answers as soon as it opens. A
// route to a stopped app answers 502; that is all that is missing.
type Ingress struct {
	Cfg config.Ingress
	Log *Journal

	session *Session
}

type IngressSnapshot struct {
	Requests int
	Failures int
}

func (i *Ingress) Snapshot() IngressSnapshot {
	requests, failures := i.session.gw.Stats(i.Cfg.Name)
	return IngressSnapshot{Requests: requests, Failures: failures}
}

// URL is the address through which the browser reaches this facade.
func (i *Ingress) URL(path string) string {
	return fmt.Sprintf("http://%s:%d%s", i.Host(), i.session.Config().Port, path)
}

// Host is the facade's host; the one that declares none answers on any, and localhost is the one
// that can be named.
func (i *Ingress) Host() string {
	if i.Cfg.Host == "" {
		return "localhost"
	}
	return i.Cfg.Host
}

type Session struct {
	// Log carries what belongs to no facade: the port, and what the private network tells.
	Log *Journal

	gw    Gateway
	net   Network
	front Front

	// watchers are the change subscribers: each its own channel, otherwise two interfaces would
	// steal each other's wake-ups. Its own lock, because a supervisor emits at any moment,
	// including while a reload holds the configuration's.
	watched  sync.Mutex
	watchers []chan Event

	// reloading serializes the re-reads: the watcher and the R key can ask for one at the same
	// time, and two interleaved evaluations would let the one that read the oldest state win.
	// frozenSaid is what was already announced as frozen, so it is only said when the declaration
	// changes — not on every save while the drift persists.
	reloading  sync.Mutex
	frozenSaid string

	// gen counts the applied reloads. The event channel can overflow and lose a wake-up; the
	// generation can be re-read at any moment — it is the one that counts for aligning.
	gen atomic.Uint64

	// mu guards what the reload replaces: the configuration, read by the proxy on each request, the
	// facades, whose logs it holds, and the list of supervisors — the declared apps, then those
	// that survive their removal as long as their process.
	mu        sync.RWMutex
	cfg       config.Config
	ingresses []*Ingress
	apps      []*App
	// viewports are the windows of the watching interfaces, and cols/rows the size lent to the
	// apps: the smallest of them. It is remembered so it is also given to the apps a reload brings
	// in.
	viewports  []*Viewport
	cols, rows int
	// running is the supervision context: Run sets it, and it is what keeps alive the apps a reload
	// declares. Before Run, nobody supervises yet.
	running context.Context
	crew    sync.WaitGroup
	// saved dates the files the configuration in force was made from: it is what the watcher
	// compares against, and it is updated on every re-read, successful or not.
	saved map[string]string
}

// NewSession builds the session, its apps and its facades. newGateway receives the session to read
// the apps' state from it: the gateway therefore never exists without something to serve.
func NewSession(cfg config.Config, network Network, front Front, newGateway func(*Session) Gateway) *Session {
	s := &Session{
		cfg:   cfg,
		cols:  defaultCols,
		rows:  defaultRows,
		net:   network,
		front: front,
		Log:   NewJournal("session", logSize, defaultRows),
	}
	s.enlist(cfg.Apps)
	s.saved = saved(cfg.Watched())
	s.adopt(cfg.Ingresses)
	s.gw = newGateway(s)
	for _, line := range cfg.Echo {
		s.Log.Add(line)
	}
	return s
}

// Generation counts the applied reloads: an interface that sees it move aligns with the
// configuration in force, even if the channel's wake-up was lost.
func (s *Session) Generation() uint64 { return s.gen.Load() }

// Watch subscribes to the session's changes: an interface redraws on it, a driver re-reads the
// state. The return value closes the subscription, and the channel with it — a subscriber that was
// waiting thus learns there is nothing left to wait for.
func (s *Session) Watch() (<-chan Event, func()) {
	ears := make(chan Event, eventBuffer)
	s.watched.Lock()
	s.watchers = append(s.watchers, ears)
	s.watched.Unlock()
	return ears, func() {
		s.watched.Lock()
		defer s.watched.Unlock()
		s.watchers = slices.DeleteFunc(s.watchers, func(other chan Event) bool { return other == ears })
		close(ears)
	}
}

// Backend returns how to reach an app, or nil if it does not listen: it is the only thing an ingress
// asks the session.
func (s *Session) Backend(name string) Backend {
	if app := s.App(name); app != nil {
		return app.Backend()
	}
	return nil
}

// Facade returns the facade that answers for this host: the one that declares it, or failing that
// the one that declares none. The proxy asks again on every request: that is how a reloaded
// configuration takes effect.
func (s *Session) Facade(host string) (config.Ingress, bool) { return s.Config().Ingress(host) }

// Config returns the configuration in force. A reload replaces it whole: reading it twice in a row
// can give two values, never a half-written one.
func (s *Session) Config() config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Facades returns the session's facades, in configuration order.
func (s *Session) Facades() []*Ingress {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ingresses
}

// Reload rereads the configuration and replaces what is declarative: the facades, their routes, their
// targets, the groups, and the list of apps. What is alive does not reload — the port is taken, the
// subnet is carved out, a process does not change command midway: an app whose declaration moved says
// so and waits for its restart, a removed app runs until it is stopped. An unreadable or invalid
// configuration changes nothing: the session carries on with the running one.
func (s *Session) Reload() error {
	s.reloading.Lock()
	defer s.reloading.Unlock()

	current := s.Config()
	s.mu.Lock()
	s.saved = saved(current.Watched())
	s.mu.Unlock()

	next, err := config.Load(current.Path, current.ForcedWatch)
	if err != nil {
		s.LogSink(s.Log)("!! " + err.Error())
		return err
	}
	// The command line's port prevails at every read: the file cannot take back a session choice, so
	// there is nothing to announce on that side.
	next.ForcedPort = current.ForcedPort
	if next.ForcedPort > 0 {
		next.Port = next.ForcedPort
	}
	stuck := frozen(current, next)
	next.Port, next.Subnet = current.Port, current.Subnet
	// The script's echo is not a declaration: it does not count in "nothing changed".
	echo := next.Echo
	next.Echo = current.Echo
	// The freeze is announced when the declaration changes, once — not on every save while the
	// drift persists, and never when the command line has the last word.
	tell := stuck != "" && stuck != s.frozenSaid
	s.frozenSaid = stuck
	if tell {
		s.Log.Addf("-- %s: the session keeps what it has until the end", stuck)
	}
	// Saving without having changed anything must neither reload the browsers nor stir the logs.
	if reflect.DeepEqual(next, current) {
		return nil
	}

	s.mu.Lock()
	s.cfg = next
	s.mu.Unlock()
	s.adopt(next.Ingresses)
	born, dropped := s.enlist(next.Apps)
	s.gen.Add(1)

	for _, line := range echo {
		s.Log.Add(line)
	}
	s.Log.Addf("-- configuration reloaded: %s, %s, %s", plural(len(next.Apps), "app"),
		plural(len(next.Ingresses), "facade"), plural(len(next.Groups), "group"))
	for _, name := range born {
		s.Log.Addf("-- %s is declared: ready to start", name)
	}
	for _, name := range dropped {
		s.Log.Addf("-- %s still runs, but nothing declares it any more: it leaves when it stops", name)
	}
	for _, app := range s.Apps() {
		if app.Drift() == Stale {
			s.Log.Addf("-- %s runs with the previous declaration: restart pending", app.Name)
		}
	}
	s.reloadBrowsers()
	s.emit(Event{Source: s.Log.Name(), Kind: Reloaded})
	return nil
}

// adopt brings the facades in line with the configuration. A facade never changes after it is
// built: the one that stays is rebuilt around the log it had, the one that appears gets a new one,
// the one that disappears leaves with its own. An interface can therefore read a facade during a
// reload — it reads the previous one, never a half-replaced one.
func (s *Session) adopt(declared []config.Ingress) {
	s.mu.Lock()
	kept := make([]*Ingress, 0, len(declared))
	hosts := make([]string, 0, len(declared))
	for _, ic := range declared {
		log := NewJournal(ic.Name, logSize, defaultRows)
		for _, known := range s.ingresses {
			if known.Cfg.Name == ic.Name {
				log = known.Log
				break
			}
		}
		kept = append(kept, &Ingress{Cfg: ic, Log: log, session: s})
		hosts = append(hosts, ic.Host)
	}
	s.ingresses = kept
	s.mu.Unlock()

	// Whatever holds the port for us must know for whom.
	if err := s.front.Hosts(hosts); err != nil {
		s.Log.Addf("!! hosts not announced: %v", err)
	}
}

// watch re-reads the configuration when one of the tool's files changes on disk: one saves in one's
// editor, the routes follow. It is not only the entry file that is watched but all its *.dsvs.ts
// neighbors — a configuration commonly imports its own. An event is only a bell, never a truth:
// touched does the comparing, which absorbs bursts, duplicates, and the directory's files that are
// none of the tool's business.
func (s *Session) watch(ctx context.Context) {
	bell, quiet := ring(s.Config().Watched())
	defer quiet()
	// What moved before the bell was set did not ring: a first look catches it, and there is no
	// blind window left between the initial read and the watch.
	if s.touched() {
		_ = s.Reload()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-bell:
			settle(ctx, bell)
		case <-time.After(watchFallback):
		}
		if s.touched() {
			_ = s.Reload()
		}
	}
}

// ring sets a kernel bell on each watched directory — the directory and not the file, because most
// editors save by renaming a temporary file over it: a bell set on the file would die with it. If
// none can be set, it never rings: the fallback poll then does all the work, as before.
func ring(paths []string) (<-chan struct{}, func()) {
	bell := make(chan struct{}, 1)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return bell, func() {}
	}
	posed := 0
	for _, dir := range dirsOf(paths) {
		if err := watcher.Add(dir); err == nil {
			posed++
		}
	}
	if posed == 0 {
		watcher.Close()
		return bell, func() {}
	}
	go func() {
		for {
			select {
			case _, live := <-watcher.Events:
				if !live {
					return
				}
				select {
				case bell <- struct{}{}:
				default:
				}
			case _, live := <-watcher.Errors:
				if !live {
					return
				}
			}
		}
	}()
	return bell, func() { _ = watcher.Close() }
}

// settle lets a save's burst finish, draining the bell while it rings.
func settle(ctx context.Context, bell <-chan struct{}) {
	deadline := time.After(settleDelay)
	for {
		select {
		case <-ctx.Done():
			return
		case <-bell:
		case <-deadline:
			return
		}
	}
}

// touched says whether the directory moved since the configuration was last read — at start, or on
// a reload, successful or not.
func (s *Session) touched() bool {
	s.mu.RLock()
	watched, seen := s.cfg.Watched(), s.saved
	s.mu.RUnlock()
	return !maps.Equal(saved(watched), seen)
}

// saved dates the watched files. A named path is dated whatever its name — it was asked for — and a
// directory only for its configuration files: a project directory is full of other .ts files that
// are none of the tool's business. An editor that saves changes the date or the size; an atomic
// replacement too, the file being recreated.
func saved(paths []string) map[string]string {
	out := map[string]string{}
	for _, path := range paths {
		info, err := os.Stat(path)
		switch {
		case err != nil:
			continue
		case !info.IsDir():
			out[path] = stamp(info)
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), config.Suffix) {
				continue
			}
			if info, err := entry.Info(); err == nil {
				out[filepath.Join(path, entry.Name())] = stamp(info)
			}
		}
	}
	return out
}

func stamp(info os.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

// dirsOf returns the directories to set a bell on: the path itself if it is one, otherwise the one
// containing the file. Without duplicates — two neighboring files are worth only one bell.
func dirsOf(paths []string) []string {
	var dirs []string
	for _, path := range paths {
		dir := path
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			dir = filepath.Dir(path)
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// reloadBrowsers asks the dev-servers to reload the page: "r" followed by enter, what their help calls
// "force reload browser". Measured: no recompilation, the page comes back as it is served — that is
// what is needed when the targets have changed, the app holding the other backend's data in memory.
func (s *Session) reloadBrowsers() {
	for _, app := range s.Apps() {
		if !app.IsReady() {
			continue
		}
		if err := app.Send("r\r"); err != nil {
			s.Log.Addf("!! %s : %v", app.Name, err)
			continue
		}
		s.Log.Addf("-- %s: reloading the browser", app.Name)
	}
}

// frozen names what a re-read configuration would like to change and the session cannot follow.
func frozen(current, next config.Config) string {
	var stuck []string
	if next.Port != current.Port {
		stuck = append(stuck, fmt.Sprintf("port %d is taken", current.Port))
	}
	if next.Subnet != current.Subnet {
		stuck = append(stuck, "the subnet is carved out")
	}
	return strings.Join(stuck, ", ")
}

// plural writes a count and its word in the plural.
func plural(n int, word string) string {
	if n != 1 {
		return strconv.Itoa(n) + " " + word + "s"
	}
	return strconv.Itoa(n) + " " + word
}

// Sink returns how to write to a facade's log, waking up the interface. Without a known name, the
// line goes to the session's log: it is about the port, not a facade.
func (s *Session) Sink() func(ingress, line string) {
	return func(name, line string) {
		for _, in := range s.Facades() {
			if in.Cfg.Name == name {
				in.Log.Add(line)
				s.emit(Event{Source: name, Kind: Logged})
				return
			}
		}
		s.LogSink(s.Log)(line)
	}
}

// Mode says which network the session obtained.
func (s *Session) Mode() Mode { return s.net.Mode() }

// Listen returns the connections of the session's port. It is the hub that holds it: nothing is
// taken here, and two sessions can therefore serve their respective hosts on the same port.
func (s *Session) Listen(port int) (net.Listener, error) { return s.front.Listen(port) }

// Apps returns the session's supervisors: the declared apps, in configuration order, then those it
// no longer declares and that still run.
func (s *Session) Apps() []*App {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.apps
}

func (s *Session) App(name string) *App {
	for _, app := range s.Apps() {
		if app.Name == name {
			return app
		}
	}
	return nil
}

// enlist aligns the list with what the configuration declares: known apps keep their supervisor,
// their log and their process, those that appear get one, and those that left the declaration move
// to the back — they stay there as long as their process lives. To be called with the configuration
// already replaced: it is the one the supervisors query.
func (s *Session) enlist(declared []config.App) (born, dropped []string) {
	s.mu.Lock()
	kept := make([]*App, 0, len(declared))
	for _, sc := range declared {
		known := false
		for _, app := range s.apps {
			if app.Name == sc.Name {
				kept, known = append(kept, app), true
				break
			}
		}
		if !known {
			app := s.recruit(sc.Name)
			kept = append(kept, app)
			born = append(born, sc.Name)
		}
	}
	for _, app := range s.apps {
		if slices.ContainsFunc(kept, func(other *App) bool { return other == app }) {
			continue
		}
		// Its process still runs: there is nothing to pull out from under whoever uses it. The
		// supervisor will withdraw itself when the app stops.
		kept = append(kept, app)
		dropped = append(dropped, app.Name)
	}
	s.apps = kept
	s.mu.Unlock()
	return born, dropped
}

// recruit builds an app's supervisor and puts it to work if the session is running. To be called with
// mu held.
func (s *Session) recruit(name string) *App {
	app := &App{
		Name:     name,
		Log:      NewJournal(name, logSize, defaultRows),
		declared: func() (config.App, bool) { return s.Config().App(name) },
		serves:   func() string { return served(s.Config(), name) },
		net:      s.net,
		cmds:     make(chan command, 8),
		notify:   s.emit,
		// The session's size, not the default one: an app born from a reload formats for the same
		// window as its neighbors.
		cols: s.cols,
		rows: s.rows,
	}
	app.dismiss = func() bool { return s.retire(app) }
	if s.running != nil {
		s.crew.Add(1)
		go func() {
			defer s.crew.Done()
			app.supervise(s.running)
		}()
	}
	return app
}

// retire removes from the session a stopped app the configuration no longer declares: it only
// survived its removal as long as its process. Returns false if it got its place back in the
// declaration, or if a command is waiting for it — one does not dismiss someone one has just spoken
// to.
func (s *Session) retire(app *App) bool {
	s.mu.Lock()
	_, live := s.cfg.App(app.Name)
	if live || len(app.cmds) > 0 {
		s.mu.Unlock()
		return false
	}
	s.apps = slices.DeleteFunc(s.apps, func(other *App) bool { return other == app })
	s.mu.Unlock()

	s.gen.Add(1)
	s.Log.Addf("-- %s leaves: nothing declares it any more", app.Name)
	s.emit(Event{Source: s.Log.Name(), Kind: Reloaded})
	return true
}

// Viewport is the window where an interface displays the logs. The apps have only one terminal each
// and several interfaces can watch the same session: the smallest announced window decides the size
// lent to them — text formatted for 70 columns reads fine in 120, the reverse gets cut.
type Viewport struct {
	session *Session
	// cols and rows live under the session's lock: it is the session that re-reads them to decide.
	cols, rows int
}

// Viewport announces a watching interface. It closes its own when leaving, otherwise the apps would
// keep the size of a window that no longer exists.
func (s *Session) Viewport() *Viewport {
	view := &Viewport{session: s}
	s.mu.Lock()
	s.viewports = append(s.viewports, view)
	s.mu.Unlock()
	return view
}

// Resize announces this window's new size. It is the sole guardian of non-positive sizes: what
// comes from here is already a size, downstream nobody checks again.
func (v *Viewport) Resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	v.session.mu.Lock()
	v.cols, v.rows = cols, rows
	v.session.mu.Unlock()
	v.session.lend()
}

// Close releases this window: the apps get back the room of the other interfaces, or the default
// one if nobody watches anymore.
func (v *Viewport) Close() {
	v.session.mu.Lock()
	v.session.viewports = slices.DeleteFunc(v.session.viewports,
		func(other *Viewport) bool { return other == v })
	v.session.mu.Unlock()
	v.session.lend()
}

// lend lends the apps the smallest announced window.
func (s *Session) lend() {
	s.mu.Lock()
	s.cols, s.rows = smallest(s.viewports)
	cols, rows, apps := s.cols, s.rows, s.apps
	s.mu.Unlock()
	for _, app := range apps {
		app.Resize(cols, rows)
	}
}

// smallest returns the smallest window, in columns and in rows separately: nobody must receive
// wider or taller than their own. When nobody watches anymore, the default size comes back — an app
// does not have to format for a closed window.
func smallest(views []*Viewport) (cols, rows int) {
	for _, view := range views {
		if view.cols <= 0 {
			continue
		}
		if cols == 0 || view.cols < cols {
			cols = view.cols
		}
		if rows == 0 || view.rows < rows {
			rows = view.rows
		}
	}
	if cols == 0 {
		return defaultCols, defaultRows
	}
	return cols, rows
}

// LogSink returns how to write to a session log, waking up the interface.
func (s *Session) LogSink(log *Journal) func(string) {
	return func(line string) {
		log.Add(line)
		s.emit(Event{Source: log.Name(), Kind: Logged})
	}
}

// Open takes the session's port, before any interface: failing here is fatal, and it is the only
// moment it can happen. The port is then held until exit, whatever the apps do — releasing it in
// between would help nobody, and it would have to be taken again for nothing.
func (s *Session) Open(ctx context.Context) error {
	if err := s.gw.Start(ctx); err != nil {
		return err
	}
	s.Log.Addf("-- listening on port %d", s.Config().Port)
	return nil
}

// Run supervises until the context is canceled, then releases the port once the apps have stopped.
// Only the named apps start: without a name, nothing is launched and the tool just waits.
func (s *Session) Run(ctx context.Context, names []string) {
	go s.watch(ctx)

	s.mu.Lock()
	s.running = ctx
	for _, app := range s.apps {
		s.crew.Add(1)
		go func() {
			defer s.crew.Done()
			app.supervise(ctx)
		}()
	}
	s.mu.Unlock()

	for _, app := range s.Apps() {
		if slices.Contains(names, app.Name) {
			app.Start()
		}
	}
	<-ctx.Done()
	s.crew.Wait()

	stopping, cancel := context.WithTimeout(context.WithoutCancel(ctx), gracePeriod)
	defer cancel()
	if err := s.gw.Stop(stopping); err != nil {
		s.Log.Addf("!! %v", err)
	}
	s.Log.Add("-- port released")
}

// emit wakes up the subscribers. Never blocking: a late subscriber loses a wake-up, not the session
// — the generation can be re-read at any moment, and it is the one that counts for aligning.
func (s *Session) emit(e Event) {
	s.watched.Lock()
	defer s.watched.Unlock()
	for _, ears := range s.watchers {
		select {
		case ears <- e:
		default:
		}
	}
}

func (s *App) send(c command) {
	select {
	case s.cmds <- c:
	default:
	}
}

// supervise alternates between two regimes: stopped, where it waits for a command, and a run, of
// which runOnce says whether another must follow.
func (s *App) supervise(ctx context.Context) {
	for again := false; ; {
		if !again {
			// Stopped and no longer declared: the app has no reason to be, and neither has its
			// supervision.
			if s.dismiss() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case c := <-s.cmds:
				if c == cmdStop {
					continue
				}
			}
		}
		again = s.runOnce(ctx)
	}
}

func (s *App) runOnce(ctx context.Context) (again bool) {
	cfg := s.Cfg()
	s.mu.Lock()
	s.run = cfg
	s.mu.Unlock()

	// The command is logged as declared: the tokens say where the network put the address, without
	// spelling out a port nobody chose.
	s.Log.Addf("$ %s", strings.Join(cfg.Command, " "))

	r, err := s.launch(cfg)
	if err != nil {
		return s.failed(err)
	}
	return s.follow(ctx, r, Snapshot{Status: Starting, PID: r.proc.Pid, Since: time.Now(),
		Restarts: s.Snapshot().Restarts})
}

// launch starts a run: an address, a real terminal, and a process group of its own.
func (s *App) launch(cfg config.App) (*run, error) {
	ep, err := s.net.Assign(s.Name)
	if err != nil {
		return nil, err
	}
	command, env := cfg.Launch(ep.IP, ep.Port)

	// The app gets a real terminal: that is what gives it colors, progress bars, and an input one
	// can write to. It is launched there as the leader of a new session, hence of a new process
	// group: a signal to the group also takes the children (npx → nx → dev-server), otherwise the
	// port would stay held by an orphan. Pdeathsig adds the case where the tool dies without being able
	// to clean up.
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = cfg.Dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if os.Getenv("TERM") == "" {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}

	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true, Pdeathsig: syscall.SIGTERM}

	s.mu.RLock()
	size := &pty.Winsize{Cols: uint16(s.cols), Rows: uint16(s.rows)}
	s.mu.RUnlock()
	tty, err := pty.StartWithAttrs(cmd, size, attrs)
	if err != nil {
		return nil, err
	}
	return &run{proc: cmd.Process, tty: tty, ep: ep}, nil
}

// follow holds a run until its end and says whether another must come.
func (s *App) follow(ctx context.Context, r *run, snap Snapshot) (again bool) {
	quietControlEcho(r.tty)

	drained := make(chan struct{})
	go s.drain(r.tty, drained)
	exited := make(chan *os.ProcessState, 1)
	go func() {
		// The run is our child. Failing to wait for it leaves a run without a status: there is nothing to get from it,
		// and it is said.
		state, err := r.proc.Wait()
		if err != nil {
			s.Log.Addf("!! pid %d: %v", r.proc.Pid, err)
		}
		exited <- state
	}()

	probeCtx, stopProbe := context.WithCancel(ctx)
	defer stopProbe()
	ready := probe(probeCtx, r.ep.String(), s.serves())
	s.publish(r, snap)

	// done is neutralized after use: a canceled context stays ready forever and the select would
	// spin.
	var (
		kill <-chan time.Time
		done = ctx.Done()
	)
	for {
		select {
		case <-done:
			done = nil
			snap.Status, kill = Stopping, s.terminate(r.proc)
			s.publish(r, snap)

		case c := <-s.cmds:
			if c == cmdStart {
				// A start during a stop means "start again afterwards": it is a restart. The app
				// already runs in the other states, there is nothing to decide.
				if snap.Status == Stopping {
					snap.Status = Restarting
					s.publish(r, snap)
				}
				continue
			}
			if snap.Status != Stopping && snap.Status != Restarting {
				kill = s.terminate(r.proc)
			}
			// The last command received decides what comes next: a stop during a restart cancels
			// it.
			snap.Status = Stopping
			if c == cmdRestart {
				snap.Status = Restarting
			}
			s.publish(r, snap)

		case <-ready:
			if snap.Status == Starting {
				s.live.Store(&backend{name: s.Name, ep: r.ep, net: s.net})
				snap.Status, snap.ReadyAt = Ready, time.Now()
				s.publish(r, snap)
			}

		case <-kill:
			s.Log.Add("!! no stop within the grace period, killing")
			s.signal(r.proc, syscall.SIGKILL)

		case state := <-exited:
			// A run ends when its process is reaped, and its group with it: `npx` launches `nx`,
			// which launches the dev-server, and that `nx` does not die with its parent — measured,
			// it survived a clean stop. Nothing in this group has a reason to outlive the run, and
			// it belongs to the run alone, the app being session leader. Taking it down closes the
			// terminal's last writer, so the read can finish: it would never finish as long as a
			// survivor holds it open, and the session would not go back to stopped.
			s.signal(r.proc, syscall.SIGKILL)
			select {
			case <-drained:
			case <-time.After(drainGrace):
				// A child that escaped the group (its own setsid) still holds the terminal: it is
				// given the buffer, not the session. Closing the terminal, deferred, unblocks the
				// read.
			}
			s.live.Store(nil)
			again = snap.Status == Restarting
			out := outcome(state, snap.Status == Stopping)
			s.Log.Addf("-- %s", out)
			if again {
				snap.Restarts++
			}
			s.publish(nil, Snapshot{Status: Idle, Last: &out, Restarts: snap.Restarts})
			r.tty.Close()
			return again
		}
	}
}

// quietControlEcho turns off the echo of control characters: without it, an arrow key a program
// does not expect would be written "^[[C" in its log.
func quietControlEcho(tty *os.File) {
	termios, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		return
	}
	termios.Lflag &^= unix.ECHOCTL
	_ = unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, termios)
}

// drain pours the app's terminal into its log until it closes — on Linux, reading a pty whose
// program is gone returns an error, which counts as end of stream.
func (s *App) drain(tty *os.File, done chan struct{}) {
	defer close(done)
	buf := make([]byte, 32*1024)
	for {
		n, err := tty.Read(buf)
		if n > 0 {
			s.Log.Write(buf[:n])
			s.notify(Event{Source: s.Name, Kind: Logged})
		}
		if err != nil {
			return
		}
	}
}

// publish replaces the current run and the published state in one go, and wakes up the interfaces.
func (s *App) publish(cur *run, snap Snapshot) {
	s.mu.Lock()
	s.cur, s.snap = cur, snap
	s.mu.Unlock()
	s.notify(Event{Source: s.Name, Kind: Changed})
}

func (s *App) failed(err error) bool {
	s.Log.Addf("!! %v", err)
	out := Outcome{Code: -1, At: time.Now()}
	s.publish(nil, Snapshot{Status: Idle, Last: &out, Restarts: s.Snapshot().Restarts})
	return false
}

func (s *App) terminate(proc *os.Process) <-chan time.Time {
	s.signal(proc, syscall.SIGTERM)
	return time.After(gracePeriod)
}

func (s *App) signal(proc *os.Process, sig syscall.Signal) {
	if err := syscall.Kill(-proc.Pid, sig); err != nil {
		_ = proc.Signal(sig)
	}
}

// served returns the path through which a facade serves this app, the first one found: that is
// where it will be asked whether it is ready.
func served(cfg config.Config, app string) string {
	for _, in := range cfg.IngressesOf(app) {
		for _, path := range in.PathsOf(app) {
			if path == "/" {
				return "/"
			}
			return path + "/"
		}
	}
	return ""
}

// probe announces only once that the app answers. It sends instead of closing: a closed channel
// would stay ready forever and make the supervisor's select spin.
func probe(ctx context.Context, addr, path string) <-chan struct{} {
	ready := make(chan struct{}, 1)
	go func() {
		for {
			if answers(ctx, addr, path) {
				ready <- struct{}{}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(probeEvery):
			}
		}
	}()
	return ready
}

// answers says whether the app is ready to serve. An open port does not prove it: a dev-server listens
// before it has compiled, and the ingress would send it requests it cannot honor yet. Where the path
// through which the app is served is known, it is therefore asked for a real response.
func answers(ctx context.Context, addr, path string) bool {
	if path == "" {
		conn, err := net.DialTimeout("tcp", addr, probeEvery)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}

	ask, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ask, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return false
	}
	res, err := prober.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	// A 404 already says the server serves: only server errors mark an app as not ready.
	return res.StatusCode < 500
}

// prober keeps no connection: the app can restart between two questions.
var prober = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// outcome says how a run ended. A missing state is a run that could not be waited for: there is no
// code to get from it.
func outcome(state *os.ProcessState, byUser bool) Outcome {
	out := Outcome{ByUser: byUser, At: time.Now()}
	if state == nil {
		out.Code = -1
		return out
	}
	out.Code = state.ExitCode()
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		out.Signal = status.Signal().String()
	}
	return out
}
