package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
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
	"dsvs/internal/projects"
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

// Outcome describes how the last run ended: on its own, by its code or a signal, or on a gesture.
// Signal is the name of the signal as one types it, SIGKILL.
type Outcome struct {
	Code   int
	Signal string
	Ending Ending
	At     time.Time
}

// Ending says what ended a run: itself, or someone's gesture — a stop, a restart.
type Ending int

const (
	ByItself Ending = iota
	Stopped
	Restarted
)

// Fell says whether the run fell: a non-zero code or a signal nobody asked for. Exiting on its own
// with code 0 is a stop, not a fall — it is the only rule, and every interface reads it here.
func (o Outcome) Fell() bool { return o.Ending == ByItself && (o.Code != 0 || o.Signal != "") }

func (o Outcome) String() string {
	switch {
	case o.Ending == Stopped:
		return "stopped"
	case o.Ending == Restarted:
		return "restarted"
	case o.Signal != "":
		return "killed by " + o.Signal
	default:
		return fmt.Sprintf("exited (%d)", o.Code)
	}
}

// Snapshot is the state of an app at a given moment. PID, Since and ReadyAt only hold in the
// statuses that carry them; Last describes the previous run when the app is Idle, and Astray says
// where a starting app listens when it does not listen at its address.
type Snapshot struct {
	Status   Status
	PID      int
	Since    time.Time
	ReadyAt  time.Time
	Last     *Outcome
	Restarts int
	Astray   *Astray
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

	// serves returns the path through which a facade serves the app, empty when none serves it. It
	// is a question, not a value — the routes may have been reloaded since the previous run.
	serves func() string

	net    Network
	cmds   chan command
	notify func(Event)

	// live carries the run's backend as soon as the app answers, and nothing otherwise: the proxy
	// has no other question to ask, and a stopped app has no representable address.
	live atomic.Pointer[backend]

	// handsOver says the session can hand over through exec: the apps then start without Pdeathsig,
	// which would kill them at the handover, the cgroup of the systemd box being the guarantee that
	// replaces it. It is a question, not a value — the gesture is made after the supervisors are
	// built.
	handsOver func() bool

	// mu guards the published state, the current run and the size lent to its terminal.
	mu   sync.RWMutex
	snap Snapshot
	// run is the declaration the last run was launched with: it is what the configuration's current
	// word is compared against, and it serves as the fallback for an app the configuration no
	// longer declares.
	run config.App
	// cur is the current run, nil when the app is stopped: its process, its terminal and its
	// address, that is everything a handover has to carry across.
	cur  *run
	cols int
	rows int
}

// run is an app's process for the duration of one supervision round. It comes from a launch or from
// the adoption of a handover, and the loop that follows it does not know which.
type run struct {
	proc *os.Process
	tty  *os.File
	ep   Endpoint
	// from is the log position where this run's output starts: what it says to declare itself ready
	// is searched from there, never in a previous run.
	from int
}

type command int

const (
	cmdStart command = iota
	cmdStop
	cmdRestart
)

func (s *App) Start() error   { return s.send(cmdStart) }
func (s *App) Stop() error    { return s.send(cmdStop) }
func (s *App) Restart() error { return s.send(cmdRestart) }

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
	case !run.SameRun(cfg):
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

	// renewal replaces the session's binary without letting go of its apps; nil when the process
	// cannot do it. Set before Run, read afterwards — see SetRenewal.
	renewal func() error

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
	mu       sync.RWMutex
	cfg      config.Config
	observed *config.Observed
	// form is the last evaluated form: the one the interfaces show. The configuration in force is
	// the last one a valid form built, so it can lag behind the form, never get ahead of it.
	form config.Form
	// notApplied says why the configuration in force is not the one the file builds today: the last
	// evaluation failed, or its form is invalid. Empty when it is. It is the only thing that says
	// so, and only the evaluation writes it.
	notApplied string
	// intents are the values chosen since the session started: a gesture writes them as soon as the
	// form accepts them, a network as soon as what it keeps applies, and they cross a handover. A
	// default is never one. awaiting are those of a draft the configuration does not apply yet:
	// they are only kept, for the project or the network, once applied.
	intents   map[string]string
	awaiting  map[string]string
	ingresses []*Ingress
	apps      []*App
	// gone are the logs of the departed apps, by name: what an app wrote can be read until the end
	// of the session, and an app of the same name that comes back takes it over.
	gone map[string]*Journal
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

// NewSession builds the session, its apps and its facades, on the configuration this form built.
// newGateway receives the session to read the apps' state from it: the gateway therefore never
// exists without something to serve.
func NewSession(form config.Form, cfg config.Config, network Network, front Front, newGateway func(*Session) Gateway) *Session {
	s := &Session{
		cfg:      cfg,
		observed: cfg.Observed,
		form:     form,
		intents:  map[string]string{},
		gone:     map[string]*Journal{},
		awaiting: map[string]string{},
		cols:     defaultCols,
		rows:     defaultRows,
		net:      network,
		front:    front,
		Log:      NewJournal("session", logSize, defaultRows),
	}
	s.hold(form)
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

// Reload replays the configuration with the session's intents, what the project keeps and the last
// observation: it is what replaces what is declarative — the facades, their routes, their targets, the
// groups, and the list of apps. What is alive does not reload — the port is taken, the subnet is carved
// out, a process does not change command midway: an app whose declaration moved says so and waits for
// its restart, a removed app runs until it is stopped. An unreadable or invalid configuration changes
// nothing: the session carries on with the running one. Neither does an invalid form, but it is the
// one the session shows from then on, with its errors. In every case it says why (NotApplied), until
// the evaluation that applies the file again — a save, a gesture, a network — and which the log tells.
func (s *Session) Reload() error { return s.replay(nil, "", config.Form.Accept) }

// Set replays the configuration with these requested values, on top of everything chosen or kept.
// It is a submission: values the form refuses, or that would make it invalid, change nothing, and
// the error says so (config.Form.Accept); the session's log says so too. A value taken becomes an
// intent of the session, and the form says where to keep it. by is the interface the gesture comes
// from — cli, panel, tui —, which the log tells along with the values taken.
func (s *Session) Set(values map[string]string, by string) error {
	return s.replay(values, by, config.Form.Accept)
}

// Draft is the gesture of an interface filling in the form, field by field: values its fields take
// become intents of the session even when they leave it invalid — a choice that requires another
// field —, and the form they make is the one the session shows, errors included. Nothing applies as
// long as it is invalid; the gesture that completes it applies everything together, and only then
// is what they chose kept. A value a field does not take is refused as by Set (config.Form.Takes).
// by is the interface the gesture comes from, as for Set.
func (s *Session) Draft(values map[string]string, by string) error {
	return s.replay(values, by, config.Form.Takes)
}

// Observe records what the machine shows, and replays the configuration if it differs from what the
// configuration was built with. The observation is kept even when the replay fails: it is a fact,
// not a declaration, and the next save of the file picks it up. The change is logged once the
// replay is done, so that the pushed state carries at once the observation and what the
// configuration derived from it.
func (s *Session) Observe(seen *config.Observed) error {
	s.mu.Lock()
	before := s.observed
	s.observed = seen
	s.mu.Unlock()
	generation, was := s.Generation(), s.Form().Shown()
	var err error
	// As long as the file does not apply, the session replays even on the network the configuration
	// in force comes from: that may be where it applies again.
	if !reflect.DeepEqual(seen, s.Config().Observed) || s.NotApplied() != "" {
		err = s.replay(nil, "", config.Form.Accept)
	}
	if !reflect.DeepEqual(seen, before) {
		outcome := "configuration unchanged"
		switch {
		case err != nil:
			outcome = "the configuration failed on it: the session keeps the previous one"
		case s.Generation() != generation:
			outcome = "configuration replayed"
		}
		// On another network, a value moves by what was kept there, or by its choices and its
		// default.
		now := s.Form().Shown()
		maps.DeleteFunc(now, func(name, value string) bool { return was[name] == value })
		if err == nil && len(now) > 0 {
			outcome += ", now " + config.Pairs(now)
		}
		s.LogSink(s.Log)(fmt.Sprintf("-- network: %s → %s, %s", before, seen, outcome))
	}
	return err
}

// Form returns the last evaluated form: what an interface shows of the parameters.
func (s *Session) Form() config.Form {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.form
}

// NotApplied says, in one line, why the configuration in force is not the one the file builds for
// what the session shows: the last evaluation failed — on a save, on the last network — or its form
// is invalid, and the targets are the previous ones. Empty when it is; the log carries the details.
func (s *Session) NotApplied() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.notApplied
}

// Chose records the values a gesture asked for at the first load, which this form accepted: what an
// accepted Set writes, for the session and for the network it observes.
func (s *Session) Chose(asked map[string]string) {
	s.take(asked)
	s.remember(s.Form(), s.Config().Path, s.Observed().Network())
}

// InputFor returns the input of an evaluation of this configuration on this network: what a gesture
// asks, what the session chose, and what the project keeps, for itself and for this network. It is
// the rule of every replay, and that of a session's first load.
func InputFor(path string, observed *config.Observed, session, asked map[string]string) config.Input {
	kept, onNetwork := projects.Recall(path, observed.Network())
	return config.Input{Asked: asked, Network: onNetwork, Session: session, Kept: kept, Observed: observed}
}

// Observed returns the last observation, for the status.
func (s *Session) Observed() *config.Observed {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.observed
}

// replay is the only path of a re-read: asked, when not empty, are values the form must have taken
// according to takes — Accept for a submission, Takes for a draft —, otherwise nothing changes, and
// by is the interface asking for them. The evaluated form is kept as soon as the evaluation
// succeeded; the configuration, only if the form is valid, and what a gesture chose is then kept.
func (s *Session) replay(asked map[string]string, by string, takes func(config.Form, map[string]string) error) error {
	s.reloading.Lock()
	defer s.reloading.Unlock()

	current := s.Config()
	s.mu.Lock()
	s.saved = saved(current.Watched())
	intents := maps.Clone(s.intents)
	s.mu.Unlock()

	observed := s.Observed()
	form, built, err := config.Load(current.Path, current.ForcedWatch, InputFor(current.Path, observed, intents, asked))
	if err != nil {
		// The log keeps the whole failure, including what the program said; whoever made the
		// gesture reads its head, like the status.
		why := headline(current.Path, err)
		if len(asked) == 0 {
			// A gesture that builds no configuration is refused, and nothing says the file itself
			// no longer applies: only an evaluation without a gesture says so.
			s.lags(why)
		}
		s.LogSink(s.Log)("!! " + err.Error())
		return errors.New(why)
	}
	if len(asked) > 0 {
		if err := takes(form, asked); err != nil {
			return s.refused(err)
		}
		s.LogSink(s.Log)("-- set " + config.Pairs(asked) + " (" + by + ")")
		s.take(asked)
	}
	s.mu.Lock()
	shown := s.form
	s.form = form
	s.mu.Unlock()
	// What the form says is logged when it starts, not on every replay while it lasts.
	for _, said := range form.Said() {
		if !slices.Contains(shown.Said(), said) {
			s.LogSink(s.Log)("-- " + said)
		}
	}
	if built == nil {
		problems := strings.Join(form.Problems(), "; ")
		s.lags(problems)
		err := errors.New("configuration not applied: " + problems)
		s.LogSink(s.Log)("-- " + err.Error())
		if len(asked) > 0 {
			// Only a draft takes a gesture that leaves the form invalid: it waits for the next one.
			return nil
		}
		return err
	}
	// Keeping is told after the replay being told, but for the network that evaluated it.
	defer s.remember(form, current.Path, observed.Network())
	s.hold(form)
	if s.lags("") != "" {
		s.LogSink(s.Log)("-- configuration applied again")
	}
	next := *built
	// The command line's port prevails at every read: the file cannot take back a session choice, so
	// there is nothing to announce on that side.
	next.ForcedPort = current.ForcedPort
	if next.ForcedPort > 0 {
		next.Port = next.ForcedPort
	}
	stuck := frozen(current, next)
	next.Port, next.Subnet = current.Port, current.Subnet
	// The script's echo, the observation and the response are not declarations: they do not count
	// in "nothing changed". What changed without the output moving reloads nothing, but the
	// configuration in force keeps the observation and the response it is made of.
	echo, observed, answer := next.Echo, next.Observed, next.Answer
	next.Echo, next.Observed, next.Answer = current.Echo, current.Observed, current.Answer
	// The freeze is announced when the declaration changes, once — not on every save while the
	// drift persists, and never when the command line has the last word.
	tell := stuck != "" && stuck != s.frozenSaid
	s.frozenSaid = stuck
	if tell {
		s.Log.Addf("-- %s: the session keeps what it has until the end", stuck)
	}
	// Saving without having changed anything must neither reload the browsers nor stir the logs.
	if reflect.DeepEqual(next, current) {
		s.mu.Lock()
		s.cfg.Observed, s.cfg.Answer = observed, answer
		s.mu.Unlock()
		if !reflect.DeepEqual(observed, current.Observed) || !reflect.DeepEqual(form, shown) {
			s.emit(Event{Source: s.Log.Name(), Kind: Changed})
		}
		return nil
	}

	next.Observed, next.Answer = observed, answer
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

// lags records why the configuration in force is no longer the file's — nothing when it is again —
// and returns what was said about it until then.
func (s *Session) lags(why string) (was string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was, s.notApplied = s.notApplied, why
	return was
}

// headline is what one line says of a failure of this configuration: its first, which starts with
// the location of the failure, written from the configuration's directory — config.dsvs.ts:12:5: …
// —, the directory being the one the session already carries. What follows is what the program
// said, which the log keeps; except when the first line only names the file: the list of its faults
// follows, and the line takes it.
func headline(path string, err error) string {
	first, rest, _ := strings.Cut(strings.TrimPrefix(err.Error(), filepath.Dir(path)+string(filepath.Separator)), "\n")
	if strings.HasSuffix(first, ":") {
		return first + " " + strings.Join(strings.Fields(rest), " ")
	}
	return first
}

// take turns values a form took into intents of the session, which wait to be applied to be kept.
func (s *Session) take(asked map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maps.Copy(s.intents, asked)
	maps.Copy(s.awaiting, asked)
}

// hold turns a value the network brought back, once applied, into the session's intent: on a
// network that kept nothing for its field, it is the one that stays, not a choice made elsewhere
// before it, what the project keeps, or the default.
func (s *Session) hold(form config.Form) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, field := range form.Fields {
		if field.From == "network" {
			s.intents[field.Name] = field.Value
		}
	}
}

// remember keeps what gestures chose, once the configuration is applied, on the form that built it:
// what it keeps for the project and for the network that evaluated it, among the values it took — a
// draft whose choice is no longer possible keeps nothing. Failing to keep takes nothing away from
// the choice, which is in force: it is logged, that is all.
func (s *Session) remember(form config.Form, path, network string) {
	s.mu.Lock()
	asked := s.awaiting
	s.awaiting = map[string]string{}
	s.mu.Unlock()
	maps.DeleteFunc(asked, func(name, value string) bool {
		field, _ := form.Field(name)
		return field.Value != value
	})
	if kept := form.Keeping(asked); len(kept) > 0 {
		s.said(kept, "kept", projects.Keep(path, kept))
	}
	if carried := form.Following(asked); len(carried) > 0 {
		s.said(carried, "kept for "+network, projects.Retain(path, network, carried))
	}
}

// said logs what a store did with these values.
func (s *Session) said(values map[string]string, kept string, err error) {
	if err != nil {
		kept = "not kept: " + err.Error()
	}
	s.LogSink(s.Log)("-- " + config.Pairs(values) + " " + kept)
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
// no longer declares and that still run. The returned list no longer changes: a reload or a removal
// sets a new one, and whoever walks it without a lock never sees a slot empty under them.
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

// Journals returns the logs of the apps these names pull in, and of all of them without a name,
// departed ones included: a departed app no longer starts by its name, but what it wrote can still
// be read there.
func (s *Session) Journals(names []string) ([]*Journal, error) {
	s.mu.RLock()
	gone := maps.Clone(s.gone)
	s.mu.RUnlock()
	var out, apps []*Journal
	for _, app := range s.Apps() {
		apps = append(apps, app.Log)
	}
	if len(names) == 0 {
		return append(apps, slices.Collect(maps.Values(gone))...), nil
	}
	var living []string
	for _, name := range names {
		if journal, left := gone[name]; left {
			out = append(out, journal)
		} else {
			living = append(living, name)
		}
	}
	wanted, err := s.Wanted(living)
	if err != nil {
		return nil, err
	}
	for _, journal := range apps {
		if slices.Contains(wanted, journal.Name()) {
			out = append(out, journal)
		}
	}
	return out, nil
}

// Wanted resolves names into the session's apps, like config.Config.Wanted, except that an app the
// configuration no longer declares and that still runs still answers to its own: that is how it is
// stopped, and how its log is read.
func (s *Session) Wanted(names []string) ([]string, error) {
	cfg := s.Config()
	var wanted []string
	for _, name := range names {
		apps, known := cfg.AppsOf(name)
		if !known && s.App(name) != nil {
			apps, known = []string{name}, true
		}
		if !known {
			return nil, fmt.Errorf("no app, facade or group named %q", name)
		}
		for _, app := range apps {
			if !slices.Contains(wanted, app) {
				wanted = append(wanted, app)
			}
		}
	}
	return wanted, nil
}

// enlist aligns the list with what the configuration declares: known apps keep their supervisor,
// their log and their process, those that appear get one, and those that left the declaration move
// to the back — they stay there as long as their process lives, and dropped names only them. To be
// called with the configuration already replaced: it is the one the supervisors query.
func (s *Session) enlist(declared []config.App) (born, dropped []string) {
	s.mu.Lock()
	kept := make([]*App, 0, len(declared))
	var idle []*App
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
		// The supervisor withdraws itself when the app stops. For one that still runs, there is
		// nothing to cut from under whoever uses it; for a stopped one, its supervisor waits for a
		// command, and a stop — which changes nothing — sends it back through the removal.
		kept = append(kept, app)
		if app.Snapshot().Status == Idle {
			idle = append(idle, app)
		} else {
			dropped = append(dropped, app.Name)
		}
	}
	s.apps = kept
	s.mu.Unlock()
	for _, app := range idle {
		app.Stop()
	}
	return born, dropped
}

// recruit builds an app's supervisor and puts it to work if the session is running. A departed app
// that comes back takes its log over. To be called with mu held.
func (s *Session) recruit(name string) *App {
	log, back := s.gone[name]
	if back {
		delete(s.gone, name)
	} else {
		log = NewJournal(name, logSize, defaultRows)
	}
	app := &App{
		Name:      name,
		Log:       log,
		declared:  func() (config.App, bool) { return s.Config().App(name) },
		serves:    func() string { return served(s.Config(), name) },
		net:       s.net,
		cmds:      make(chan command, 8),
		notify:    s.emit,
		handsOver: s.handsOver,
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
// survived its removal as long as its process, and its log stays. Returns false if it got its place
// back in the declaration, or if a command is waiting for it — one does not dismiss someone one has
// just spoken to.
func (s *Session) retire(app *App) bool {
	s.mu.Lock()
	_, live := s.cfg.App(app.Name)
	if live || len(app.cmds) > 0 {
		s.mu.Unlock()
		return false
	}
	s.apps = slices.DeleteFunc(slices.Clone(s.apps), func(other *App) bool { return other == app })
	s.gone[app.Name] = app.Log
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

// send hands a command to the supervisor, without waiting for it. A full queue refuses it and says
// so: it is a gesture someone made, and it is not lost silently.
func (s *App) send(c command) error {
	select {
	case s.cmds <- c:
		return nil
	default:
		return fmt.Errorf("%s already has %d orders waiting", s.Name, cap(s.cmds))
	}
}

// supervise alternates between two regimes: stopped, where it waits for a command, and a run, of
// which runOnce says whether another must follow. An app adopted from a handover already has its
// run: supervision resumes there, launching nothing and waiting for nobody.
func (s *App) supervise(ctx context.Context) {
	for again := s.resume(ctx); ; {
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
	// port would stay held by an orphan.
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = cfg.Dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if os.Getenv("TERM") == "" {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}

	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if !s.handsOver() {
		// Pdeathsig adds the case where the tool dies without being able to clean up. Where the
		// session can hand over, it is redundant and fatal: PR_SET_PDEATHSIG fires on the death of
		// the thread that forked, hence at every handover exec — measured, 2 apps surviving out of
		// 20. The cgroup of the systemd box then covers the same thing, and better.
		attrs.Pdeathsig = syscall.SIGTERM
	}

	s.mu.RLock()
	size := &pty.Winsize{Cols: uint16(s.cols), Rows: uint16(s.rows)}
	s.mu.RUnlock()
	_, from, _ := s.Log.Since(math.MaxInt)
	tty, err := pty.StartWithAttrs(cmd, size, attrs)
	if err != nil {
		return nil, err
	}
	return &run{proc: cmd.Process, tty: tty, ep: ep, from: from}, nil
}

// follow holds a run until its end and says whether another must come. It does not know whether the
// run comes from a launch or an adoption: snap says where it stands, and live whether the app
// already answers — in which case the proxy keeps its address and there is nothing to probe.
func (s *App) follow(ctx context.Context, r *run, snap Snapshot) (again bool) {
	quietControlEcho(r.tty)

	drained := make(chan struct{})
	go s.drain(r.tty, drained)
	exited := make(chan *os.ProcessState, 1)
	go func() {
		// The run is our child, a handover's exec changing neither our PID nor the parentage.
		// Failing to wait for it leaves a run without a status: there is nothing to get from it,
		// and it is said.
		state, err := r.proc.Wait()
		if err != nil {
			s.Log.Addf("!! pid %d: %v", r.proc.Pid, err)
		}
		exited <- state
	}()

	probeCtx, stopProbe := context.WithCancel(ctx)
	defer stopProbe()
	var (
		ready  <-chan struct{}
		astray <-chan *Astray
	)
	if s.live.Load() == nil {
		isReady, strays := s.awaiting(r)
		ready, astray = probe(probeCtx, isReady, strays)
	}
	s.publish(r, snap)

	// done is neutralized after use: a canceled context stays ready forever and the select would
	// spin.
	var (
		kill <-chan time.Time
		done = ctx.Done()
	)
	if snap.Status == Stopping || snap.Status == Restarting {
		// A stop requested before the handover resumes its deadline: without it, nobody reaps
		// anymore.
		kill = time.After(gracePeriod)
	}
	for {
		select {
		case <-done:
			done = nil
			snap.Status, snap.Astray, kill = Stopping, nil, s.terminate(r.proc)
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
			snap.Status, snap.Astray = Stopping, nil
			if c == cmdRestart {
				snap.Status = Restarting
			}
			s.publish(r, snap)

		case <-ready:
			if snap.Status == Starting {
				s.live.Store(&backend{name: s.Name, ep: r.ep, net: s.net})
				snap.Status, snap.ReadyAt, snap.Astray = Ready, time.Now(), nil
				s.publish(r, snap)
			}

		case seen := <-astray:
			if snap.Status == Starting {
				snap.Astray = seen
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
			out := outcome(state, ending(snap.Status))
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

// publish replaces the current run and the published state in one go, and wakes up the interfaces:
// a run without a published state, or the reverse, would be a state a handover could not describe.
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

// awaiting returns the two questions asked of a starting run: is it ready, according to the
// declaration it was launched with, and where does it listen if not at its address. Its ready key
// decides when present; otherwise its command and the current routes do: an app a facade serves is
// ready when a real request gets a response, an app that receives a port without a route when the
// port opens, an app that receives none as soon as it is launched — it has neither an address to
// get wrong, nor anything to wait for.
func (s *App) awaiting(r *run) (ready func(context.Context) bool, astray func() *Astray) {
	s.mu.RLock()
	cfg := s.run
	s.mu.RUnlock()
	addr := r.ep.String()
	astray = func() *Astray { return strayed(r.proc.Pid, r.ep) }
	if !cfg.Listens() {
		astray = func() *Astray { return nil }
	}
	switch path := s.serves(); {
	case cfg.Ready != nil && cfg.Ready.Log != nil:
		return printed(s.Log, r.from, *cfg.Ready.Log), astray
	case cfg.Ready != nil:
		return func(ctx context.Context) bool { return asked(ctx, addr, cfg.Ready.HTTP)/100 == 2 }, astray
	case path != "":
		// A 404 already says the server serves: only server errors mark an app as not ready.
		return func(ctx context.Context) bool { code := asked(ctx, addr, path); return code != 0 && code < 500 }, astray
	case !cfg.Listens():
		return func(context.Context) bool { return true }, astray
	}
	return func(context.Context) bool { return opens(addr) }, astray
}

// probe announces only once that the run is ready. It sends instead of closing: a closed channel
// would stay ready forever and make the supervisor's select spin. Until then, it says where the run
// listens when it is not at its address, each time that changes.
func probe(ctx context.Context, isReady func(context.Context) bool, strays func() *Astray) (<-chan struct{}, <-chan *Astray) {
	ready, astray := make(chan struct{}, 1), make(chan *Astray)
	go func() {
		var said *Astray
		for {
			if isReady(ctx) {
				ready <- struct{}{}
				return
			}
			if seen := strays(); !reflect.DeepEqual(seen, said) {
				said = seen
				select {
				case astray <- seen:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(probeEvery):
			}
		}
	}()
	return ready, astray
}

// asked returns the code a real request gets on this path, 0 without a response. An open port does
// not prove that an app serves: a dev-server listens before it has compiled, and the ingress would
// send it requests it cannot honor yet.
func asked(ctx context.Context, addr, path string) int {
	ask, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ask, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return 0
	}
	res, err := prober.Do(req)
	if err != nil {
		return 0
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// opens says whether the port opens: it is all that can be asked of an app no facade serves.
func opens(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, probeEvery)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// printed returns the question "does a line of its output say it is ready?", asked of the lines the
// log completes from from on, styles stripped. The pattern was validated with the configuration.
func printed(journal *Journal, from int, pattern config.Pattern) func(context.Context) bool {
	expected, _ := pattern.Compile()
	return func(context.Context) bool {
		lines, next, _ := journal.Since(from)
		from = next
		return slices.ContainsFunc(lines, func(line Line) bool { return expected.MatchString(styles.ReplaceAllString(line.Text, "")) })
	}
}

// styles are the style sequences a log returns in its lines.
var styles = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// prober keeps no connection: the app can restart between two questions.
var prober = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// outcome says how a run ended. A missing state is a run that could not be waited for: there is no
// code to get from it.
func outcome(state *os.ProcessState, ending Ending) Outcome {
	out := Outcome{Ending: ending, At: time.Now()}
	if state == nil {
		out.Code = -1
		return out
	}
	out.Code = state.ExitCode()
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		out.Signal = unix.SignalName(status.Signal())
	}
	return out
}

// ending says what ends a run that exits in this status: a requested stop or restart, or nothing
// but itself.
func ending(status Status) Ending {
	switch status {
	case Stopping:
		return Stopped
	case Restarting:
		return Restarted
	}
	return ByItself
}
