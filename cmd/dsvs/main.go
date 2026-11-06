package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"

	"dsvs/internal/attach"
	"dsvs/internal/build"
	"dsvs/internal/config"
	"dsvs/internal/core"
	"dsvs/internal/gateway"
	"dsvs/internal/hub"
	"dsvs/internal/netns"
	"dsvs/internal/projects"
	"dsvs/internal/ui/panel"
	"dsvs/internal/ui/plain"
	"dsvs/internal/ui/tray"
	"dsvs/internal/update"
)

func main() {
	s := &shape{net: "auto", shift: portShift()}
	fs := newFlagSet("dsvs")
	s.declare(fs)
	// The plumbing the binary addresses to itself stays as flags, and is not a command: the installed
	// unit, the hub, spawn, build.sh and update.VersionOf type them — including an old binary during an
	// update, which will never know the commands of this one.
	ports := fs.Bool("hub", false, "hold the ports for the sessions, without serving anything")
	headless := fs.Bool("session", false, "hold the session without an interface, for the clients that attach to it")
	icon := fs.Bool("tray", false, "hold the status bar icon, which opens the panel on a click")
	unit := fs.Bool("install-unit", false, "install the systemd --user unit that starts the service at session start")
	noUnit := fs.Bool("remove-unit", false, "remove the systemd --user unit of this binary, leaving its service running")
	tell := fs.Bool("version", false, "write the version of the binary and quit")
	readme := fs.String("readme", "", "`path` of a README whose dsvs help block is rewritten from this binary's help")
	switch err := fs.Parse(os.Args[1:]); {
	case errors.Is(err, flag.ErrHelp):
		general(os.Stdout, s)
		return
	case err != nil:
		die(fmt.Errorf("%w (%s help)", err, build.Name()))
	}

	// The role is a single decision point: one flag names it, two are refused, and none lets the
	// command line decide. None of these roles needs a configuration or a running service: the hub is
	// already in its unit, the icon holds the display, and the unit is installed once and for all.
	chosen, err := only([]role{
		{"version", *tell, func() error { fmt.Println(build.Version); return nil }},
		{"hub", *ports, hub.Serve},
		{"tray", *icon, hold},
		{"install-unit", *unit, func() error { return installUnit(s.shift) }},
		{"remove-unit", *noUnit, removeUnit},
		{"session", *headless, func() error { return serve(true, s, fs.Args()) }},
		{"readme", *readme != "", func() error { return refreshREADME(*readme) }},
	}, func() error { return dispatch(s, fs.Args()) })
	if err != nil {
		die(err)
	}
	if err := chosen(); err != nil {
		die(err)
	}
}

// serve holds the session of this configuration in this process: the systemd box first, then the
// private network, then the session. headless leaves it without a terminal, for the clients that attach
// to it; otherwise the logs stream onto this one.
func serve(headless bool, s *shape, names []string) error {
	// An inherited manifest says a running session is being taken over: its box, its private network
	// and its apps are already there, and none of it is set up again. If it is unreadable, there is
	// nothing to take over: exit, and the cgroup takes the apps away rather than leaving them with
	// nobody.
	taken, err := taking()
	if err != nil {
		return err
	}
	// Three situations: this is the supervisor, already in its private network; this tries to open it,
	// and everything then plays out in the re-executed binary; or this does without it, keeping why.
	ctl, inside := os.LookupEnv(netns.CtlEnv)
	// The box before anything else: gatekeeper, supervisor and apps must be born in it. The
	// supervisor does not have to ask — it is already in its gatekeeper's box, and above all in
	// namespaces that systemd-run could not join.
	if !inside {
		box(headless)
	}
	fallback := ""
	switch {
	case taken != nil:
		// The gatekeeper still waits for this PID, and the network is set up: only the reason for doing
		// without it is taken over, so that the network's card keeps telling the same story.
		fallback = taken.Fallback
	case inside:
	case s.net == "loopback":
		fallback = "asked for by hand"
	default:
		code, err := netns.Gate(os.Getenv(boxEnv) == inBox)
		switch {
		case err == nil && code != netns.Unreachable:
			os.Exit(code)
		case s.net == "netns":
			return fmt.Errorf("private network impossible: %w", cause(err))
		default:
			fallback = cause(err).Error()
		}
	}
	return run(s, headless, ctl, fallback, names, taken)
}

// hold holds the status bar icon. Only one at a time, and the socket says so: taking it fails when
// another one holds it, as for the hub. This socket is also the voice of the service: it is through it
// that the panel asks the service to go away.
func hold() error {
	// The service is born from the systemd manager and not from a shell: it takes the environment of
	// one of the user's terminals itself, once, before anything descends from it.
	takeLoginEnv()
	sock, err := hub.Bind(hub.TrayAddr())
	if errors.Is(err, hub.ErrTaken) {
		return nil // an icon is already up: nothing to do, and nothing to say
	}
	if err != nil {
		return err
	}
	defer sock.Close()
	go listen(sock)
	return tray.Run(panel.Open)
}

// listen listens to what is asked of the service. One word, "quit", and it goes away: the icon has
// nothing to save, and the panel that asked may live in this very process — waiting for it would lead
// nowhere.
func listen(sock net.Listener) {
	for {
		conn, err := sock.Accept()
		if err != nil {
			return
		}
		asked, _ := bufio.NewReader(conn).ReadString('\n')
		switch strings.TrimSpace(asked) {
		case "quit":
			conn.Close()
			os.Exit(0)
		case "restart":
			// The service replaces itself with the binary on disk, the one the update just put there:
			// same PID, so the systemd unit that holds it sees nothing happen, and the socket closes at
			// the exec to be taken again right away. An exec that succeeds never returns: what follows
			// only happens on failure, and that is what the update waits for — without it, it would
			// only have the service's word that it moved to the new binary.
			if err := relaunch(); err != nil {
				fmt.Fprintln(conn, err)
			}
		}
		conn.Close()
	}
}

// relaunch replaces this process with the binary on disk, and only returns the reason it could not.
func relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, []string{exe, "--tray"}, os.Environ())
}

// upgrade takes a newer version if there is one. Look first: the update rebuilds or downloads, and
// none of that is needed when the right one is already there.
func upgrade() error {
	newer, err := update.Newer()
	if err != nil {
		return err
	}
	if newer == "" {
		fmt.Println("already up to date: " + build.Label())
		return nil
	}
	fmt.Println(build.Name() + " " + build.Version + " → " + newer)
	if err := update.Apply(func(step string) { fmt.Println("-- " + step) }); err != nil {
		return err
	}
	return nil
}

// writeTypes writes the pnpm specifier of the types that go with this binary, and says on errOut when
// the binary is not exactly on its tag: the package is then the one of the last tag.
func writeTypes(out, errOut io.Writer) error {
	tag, exact, err := release(build.Version)
	if err != nil {
		return err
	}
	if !exact {
		fmt.Fprintf(errOut, "%s is ahead of its last tag: writing the types of %s\n", build.Version, tag)
	}
	_, err = fmt.Fprintf(out, "git+ssh://git@github.com/%s.git#%s&path:/types\n", update.Repo(), tag)
	return err
}

var described = regexp.MustCompile(`^(v\d+\.\d+\.\d+)(-\d+-g[0-9a-f]+)?(-dirty)?$`)

// release returns the tag this binary comes from, and whether the binary is exactly that tag: git
// describe adds the number of commits ahead and -dirty to the version of a clone.
func release(version string) (tag string, exact bool, err error) {
	parts := described.FindStringSubmatch(version)
	if parts == nil {
		return "", false, errors.New("this binary carries no version: pin the types by hand")
	}
	return parts[1], parts[2] == "" && parts[3] == "", nil
}

// machine is what runs here: this binary, its service, the sessions whose port it holds, the projects
// it remembers, and the other dsvs binaries with theirs. A single read, two renderings — the text and
// the JSON say the same thing.
type machine struct {
	Binary     string        `json:"binary"`
	Version    string        `json:"version"`
	Service    bool          `json:"service"`
	Sessions   []hub.Session `json:"sessions"`
	Remembered []string      `json:"remembered"`
	Peers      []hub.Peer    `json:"peers"`
}

// MarshalJSON writes every list, empty ones included, like the state of a session.
func (m machine) MarshalJSON() ([]byte, error) {
	type plain machine
	m.Sessions, m.Remembered, m.Peers = append([]hub.Session{}, m.Sessions...), append([]string{}, m.Remembered...), append([]hub.Peer{}, m.Peers...)
	return json.Marshal(plain(m))
}

// status writes what runs. It is the answer to "what holds my ports", and it needs neither a
// configuration nor a running service.
func status(out io.Writer, asJSON bool) error {
	sessions, err := hub.Sessions()
	if err != nil {
		return err
	}
	here := machine{
		Binary:     build.Name(),
		Version:    build.Version,
		Service:    hub.Answers(hub.TrayAddr()),
		Sessions:   sessions,
		Remembered: projects.List(),
		Peers:      hub.Peers(),
	}
	if asJSON {
		return write(out, here)
	}
	writeStatus(out, here)
	return nil
}

// remembering remembers the project of the configuration here, without opening a session.
func remembering(out io.Writer, s *shape, names []string) error {
	config, err := projectHere("remember", s, names)
	if err != nil {
		return err
	}
	if slices.Contains(projects.List(), config) {
		fmt.Fprintf(out, "%s is already remembered\n", config)
		return nil
	}
	if err := projects.Remember(config); err != nil {
		return err
	}
	fmt.Fprintf(out, "remembered %s\n", config)
	return nil
}

// forgetting forgets the project of the configuration here, once its apps are off. A session open for
// it with no app running stays open: this is said, since the panel will no longer list the project once
// the session is gone from it, and quit is then the only way to close the session.
func forgetting(out io.Writer, s *shape, names []string) error {
	config, err := projectHere("forget", s, names)
	if err != nil {
		return err
	}
	listed, valued, err := attach.Forget(config)
	switch {
	case err != nil:
		return err
	case listed && valued:
		fmt.Fprintf(out, "forgot %s and the values kept for it", config)
	case listed:
		fmt.Fprintf(out, "forgot %s", config)
	default:
		fmt.Fprintf(out, "%s was not remembered; %s status lists what is\n", config, build.Name())
		return nil
	}
	if living, err := hub.Sessions(); err == nil &&
		slices.ContainsFunc(living, func(session hub.Session) bool { return session.Config == config }) {
		fmt.Fprintf(out, "; its session stays open on the values chosen in it, %s quit closes it", build.Name())
	}
	fmt.Fprintln(out)
	return nil
}

// projectHere is the project that --config names, or that the configuration found here identifies: the
// only way to name a project, hence no argument.
func projectHere(verb string, s *shape, names []string) (string, error) {
	if len(names) > 0 {
		return "", fmt.Errorf("%s takes no argument, got %q; name the project with --config", verb, strings.Join(names, " "))
	}
	return resolve(s.config)
}

// writeStatus writes what runs for a human. It is a rendering and nothing else.
func writeStatus(out io.Writer, here machine) {
	fmt.Fprintf(out, "%s %s\n", here.Binary, here.Version)
	if here.Service {
		fmt.Fprintln(out, "  service: running, icon in the status bar")
	} else {
		fmt.Fprintln(out, "  service: not running")
	}
	if len(here.Sessions) == 0 {
		fmt.Fprintln(out, "  sessions: none, no port held")
	}
	for _, session := range here.Sessions {
		fmt.Fprintf(out, "  port %d  %s\n", session.Port, session.Config)
	}
	if len(here.Remembered) == 0 {
		fmt.Fprintln(out, "  remembered: none")
	}
	for _, config := range here.Remembered {
		fmt.Fprintf(out, "  remembered  %s\n", config)
	}
	for _, peer := range here.Peers {
		fmt.Fprintf(out, "%s%s\n", peer.Name, since(peer))
		for _, session := range peer.Sessions {
			fmt.Fprintf(out, "  port %d  %s\n", session.Port, session.Config)
		}
	}
}

// since says of another binary what it was willing to say of itself: its version, since when it has
// held the ports. A hub too old to answer does not make the line false, only shorter.
func since(peer hub.Peer) string {
	var said []string
	if peer.Version != "" {
		said = append(said, peer.Version)
	}
	if !peer.Since.IsZero() {
		said = append(said, "holding ports since "+peer.Since.Format("15:04"))
	}
	if len(said) == 0 {
		return ""
	}
	return " (" + strings.Join(said, ", ") + ")"
}

// held names what already holds this port, when another dsvs binary opened it: it is the only thing
// "address already in use" lacks for one to know what to do. A port taken by anything else has
// nothing to add.
func held(port int) string {
	peer, session, ok := hub.Holder(port)
	if !ok {
		return ""
	}
	return fmt.Sprintf("; %s%s holds it for %s", peer.Name, since(peer), session.Config)
}

// service is the command: without an argument it ensures the background service and its icon, so a
// missing status bar is a failure here; stop and restart go through the icon's socket, which is the
// voice of the service and hears them wherever they come from.
func service(args []string) error {
	switch {
	case len(args) == 0:
		started, err := raise()
		switch {
		case err != nil:
			return err
		case started:
			_, err = fmt.Println("service started")
		default:
			_, err = fmt.Println("service already running")
		}
		return err
	case len(args) == 1 && args[0] == "stop":
		if err := say("quit"); err != nil {
			return err
		}
		_, err := fmt.Println("service stopped")
		return err
	case len(args) == 1 && args[0] == "restart":
		if err := say("restart"); err != nil {
			return err
		}
		if !hub.Awaits(hub.TrayAddr()) {
			return errors.New("the service did not put its icon back in the status bar")
		}
		_, err := fmt.Println("service restarted on " + build.Label())
		return err
	}
	return fmt.Errorf("service takes stop, restart, or nothing, got %q", strings.Join(args, " "))
}

// say carries a word to the icon's socket and returns what it had to answer: nothing when the word
// was obeyed — the service goes away or execs, and the socket closes on its silence.
func say(word string) error {
	conn, err := net.Dial("unix", hub.TrayAddr())
	if err != nil {
		return errors.New("no service running")
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, word); err != nil {
		return err
	}
	answer, _ := bufio.NewReader(conn).ReadString('\n')
	if answer = strings.TrimSpace(answer); answer != "" {
		return errors.New(answer)
	}
	return nil
}

// openPanel opens the projects panel without going through the icon. The null rectangle says no icon
// anchors it: it places itself.
func openPanel() error {
	return panel.Open(image.Rect(-1, -1, -1, -1), nil, func() {})
}

// drawPanel draws the panel into a PNG file, without a window: what an agent, a bug report or an SSH
// session can see of the panel.
func drawPanel(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := panel.Render(file); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// raise makes sure the service runs, and says whether it started it: the hub of the ports is born on
// demand from the sessions, the status bar icon is what remains to hold. The gesture can be repeated
// harmlessly — what to say about it belongs to whoever asked.
func raise() (bool, error) {
	if hub.Answers(hub.TrayAddr()) {
		return false, nil
	}
	if !tray.Available() {
		return false, errNoStatusBar
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	// The service does not belong to the terminal that asked for it: it lives until the X session ends.
	holder := exec.Command(exe, "--tray")
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := holder.Start(); err != nil {
		return false, err
	}
	// A started service implies the icon is up: wait for it to answer rather than promise it.
	if !hub.Awaits(hub.TrayAddr()) {
		return false, errors.New("the service did not put its icon in the status bar")
	}
	return true, nil
}

// errNoStatusBar says there is no icon to put up on this display. It is a failure for whoever asked
// only for the service, a mere note for the session that does without it.
var errNoStatusBar = errors.New("no status bar on this display; the panel opens with: " + build.Name() + " panel")

// lend lends the terminal of this process to the session of the configuration here, starting it if it
// is not there yet, and the service with it. names are the apps to start, ordered after attaching.
func lend(s *shape, names []string) error {
	config, err := resolve(s.config)
	if err != nil {
		return err
	}
	if err := serving(); err != nil {
		return err
	}
	return attach.Attach(config, ui(), names, func() error { return spawn(config, s.args()) })
}

// serving ensures the service for whoever wants a session: what it changed is said, and a missing
// status bar is only a note here — a session serves without an icon.
func serving() error {
	started, err := raise()
	switch {
	case errors.Is(err, errNoStatusBar):
		fmt.Println(err)
	case err != nil:
		return err
	case started:
		fmt.Println("service started")
	}
	return nil
}

// ui is the interface this terminal can render: the TUI needs a real one, the log stream makes do with
// a pipe.
func ui() string {
	if term.IsTerminal(os.Stdout.Fd()) {
		return "tui"
	}
	return "plain"
}

// spawn starts the session without an interface. It puts itself in its box and its network and returns
// right away: anything that could then prevent it from serving is said through its socket, to whoever
// waits for it.
func spawn(config string, shape []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	session := exec.Command(exe, append([]string{"--session", "--config", config}, shape...)...)
	session.Stderr = os.Stderr
	// A session does not belong to the terminal that asked for it: otherwise, a ctrl+c in the client
	// would take it down too.
	session.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return session.Start()
}

// cause names the failure of the private network. Without an error, the supervisor gave up, having
// already said why on stderr.
func cause(err error) error {
	if err == nil {
		return errors.New("the private network could not be set up")
	}
	return err
}

func die(err error) {
	if !errors.Is(err, errSaid) {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(1)
}

// errSaid is the failure of a session that already said why, on its socket, to the client waiting for
// it. Outside a systemd box, its stderr is that of the terminal that started it: saying it there again
// would write it twice.
var errSaid = errors.New("the session said why on its socket")

func run(s *shape, headless bool, ctl, fallback string, args []string, taken *core.Handover) error {
	path, err := resolve(s.config)
	if err != nil {
		return err
	}
	names, asked, err := namesAndValues(args)
	if err != nil {
		return err
	}
	// The session's socket opens before anything is read: it is named by the configuration, which is
	// the identity of the session, and it is through it that a client will learn the session did not
	// start — otherwise it would only have a silent wait.
	var attachers *attach.Server
	if headless {
		server, err := bind(path, taken)
		if errors.Is(err, hub.ErrTaken) {
			return nil // another session already serves this configuration: nothing to do, and nothing to say
		}
		if err != nil {
			return err
		}
		defer server.Close()
		attachers = server
	}
	failed := func(err error) error {
		if attachers != nil && attachers.Fail(err) {
			return errSaid
		}
		return err
	}

	serving, stop := context.WithCancel(context.Background())
	defer stop()

	// The subscription comes before the configuration is read: it receives the network as it is, and
	// the apps start on the right target instead of waiting for a replay. Its log lines wait for the
	// session.
	notes := make(chan string, 4)
	note := func(line string) {
		select {
		case notes <- line:
		default:
		}
	}
	observed, later := observe(serving, ctl, note)

	form, cfg, err := opened(path, s, observed, taken, asked)
	if err != nil {
		return failed(err)
	}
	names, err = cfg.Wanted(names)
	if err != nil {
		return failed(fmt.Errorf("%s: %w", cfg.Path, err))
	}
	if taken != nil {
		// A handover starts nothing: what was running keeps running, and what was stopped stays stopped.
		names = nil
	}

	network, err := join(cfg, ctl, fallback, taken)
	if err != nil {
		// The gatekeeper waits for this code to fall back on loopback: only it can still do so.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(netns.Unreachable)
	}

	// The port no longer opens here: the hub holds it for every session, and hands each one the
	// connections of the hosts it declares. That is what lets them coexist on 4200. The configuration is
	// the identity of the session: the hub names it by it, and refuses a second session of the same file
	// by it.
	front := hub.New(cfg.Path, network.Mode().Isolated)
	session := core.NewSession(form, cfg, network, front, func(s *core.Session) core.Gateway {
		return gateway.NewProxy(cfg.Port, s, s.Sink())
	})
	session.SetRenewal(renewal(session, attachers, network))
	if taken == nil {
		// A handover already said what the form says, in the log it carries.
		for _, said := range form.Said() {
			session.Log.Add("-- " + said)
		}
	}
	if observed != nil {
		note("-- network: " + observed.String() + recalled(form))
	}
	session.Chose(asked)

	// The apps of the handover become this session's own again before the proxy opens: those that
	// answered still answer, and no request hits a 502 for the time of a probe. Failing the adoption
	// is the only case where exiting is preferred: apps nobody supervises any more cannot be recovered.
	if taken != nil {
		if err := session.Adopt(*taken); err != nil {
			return failed(fmt.Errorf("handover: %w", err))
		}
	}

	// A session without a box does not have the cgroup guarantee: a brutal stop can leave orphans.
	// That is said, with the reason.
	if reason := os.Getenv(boxEnv); reason != inBox && reason != "" {
		session.Log.Addf("-- no systemd box (%s): a brutal stop may leave orphan apps", reason)
	}

	// A network that has output tells it: its lines go to the session's log, including the
	// reservations it has about its own setup.
	if teller, ok := network.(interface {
		Watch(func(string))
		Warnings() []string
	}); ok {
		for _, warning := range teller.Warnings() {
			session.Log.Add(warning)
		}
		go teller.Watch(session.LogSink(session.Log))
	}

	front.Say(session.LogSink(session.Log))

	go func() {
		for line := range notes {
			session.Log.Add(line)
		}
	}()
	go func() {
		for change := range later {
			_ = session.Observe(&change) // a replay that fails is already said in the log
		}
	}()

	// The port is taken here, before any interface: when busy, it is an error one reads, not a log line
	// in an interface that pretends to work. It is changed with --port.
	if err := session.Open(serving); err != nil {
		return failed(fmt.Errorf("port %d: %w%s", cfg.Port, err, held(cfg.Port)))
	}
	// The project is remembered as soon as its session has started: that is what makes it appear in
	// the panel, cold, at the next start. A refused session remembers nothing. Failing to write it does
	// not prevent serving.
	_ = projects.Remember(path)

	done := make(chan struct{})
	go func() {
		defer close(done)
		session.Run(serving, names)
	}()

	// A SIGTERM stops cleanly whatever the interface — that is what makes the session killable from
	// outside the private network. In plain, ctrl+c adds to it; in the TUI it is a key.
	ctx, release := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer release()

	if headless {
		attachers.Ready(ctx, session, stop)
		// Without an interface, the session waits: for a signal, or for an interface that asked to
		// close everything. Clients come and go in the meantime.
		select {
		case <-ctx.Done():
		case <-serving.Done():
		}
	} else {
		// dsvs run holds the session here, logs streaming: the shape of scripts, where ctrl+c must
		// stop everything.
		ctx, release := signal.NotifyContext(ctx, os.Interrupt)
		defer release()
		plain.Run(ctx, session, os.Stdout)
	}

	stop()
	<-done
	return nil
}

// opened reads the configuration for the first time, under the rule of every replay: what run asks
// for, then what the handover had chosen, and what the project keeps. A new session thus starts on what
// had been chosen in it, with nothing to restart. An invalid form builds nothing, and there is no
// configuration in effect to fall back on: the error says how to start. A handover does have one, and
// resumes from it without evaluating anything: an update therefore does not lose a session over a file
// the new binary refuses.
func opened(path string, s *shape, observed *config.Observed, taken *core.Handover, asked map[string]string) (config.Form, config.Config, error) {
	var intents map[string]string
	if taken != nil {
		intents = taken.Params
	}
	form, cfg, err := load(taken.Load, path, s.port, s.shift, s.watch, core.InputFor(path, observed, intents, asked))
	if err != nil {
		return form, config.Config{}, err
	}
	if err := form.Accept(asked); err != nil {
		return form, config.Config{}, fmt.Errorf("%s: %w; start with %s run name=value, or %[3]s forget", path, err, build.Name())
	}
	return form, *cfg, nil
}

// recalled says, for the starting line, the values a memory of the project brought back.
func recalled(form config.Form) string {
	back := map[string]string{}
	for _, field := range form.Fields {
		if !field.Hidden && (field.From == "network" || field.From == "kept") {
			back[field.Name] = field.Value
		}
	}
	if len(back) == 0 {
		return ""
	}
	return ", back to " + config.Pairs(back)
}

// namesAndValues splits what run receives: names of apps, facades or groups, and name=value values,
// asked for at the first read as set asks for them afterwards.
func namesAndValues(args []string) ([]string, map[string]string, error) {
	var names, pairs []string
	for _, arg := range args {
		if strings.Contains(arg, "=") {
			pairs = append(pairs, arg)
		} else {
			names = append(names, arg)
		}
	}
	values, err := valued("run", pairs)
	return names, values, err
}

// observe returns the network as it is at the start, and the channel of its changes. Without a first
// value within two seconds, the configuration starts without an observation and the next one will
// replay it: better to start than to block on netlink.
func observe(ctx context.Context, ctl string, line func(string)) (*config.Observed, <-chan config.Observed) {
	values, err := netns.Observe(ctx, ctl, line)
	if err != nil {
		line("-- network not observed: " + err.Error())
		return nil, nil
	}
	select {
	case first, open := <-values:
		if open {
			return &first, values
		}
	case <-time.After(2 * time.Second):
		line("-- network not observed: no answer in 2s")
	}
	return nil, values
}

// resolve returns the path of the configuration, the one that identifies the session: the one named,
// or the first found walking up from the current directory. A client and its session compute it the
// same way, and that is what makes them find each other.
func resolve(path string) (string, error) {
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		if path, err = config.Find(cwd); err != nil {
			return "", err
		}
	}
	return filepath.Abs(path)
}

// loader reads a configuration: by evaluating it (config.Load), or from the answer a handover carries.
type loader func(path string, watch []string, in config.Input) (config.Form, *config.Config, error)

// load reads the configuration through read and applies the session's choices to the one the form
// builds: the port, and the paths to watch on top of its own. The two ways of choosing the port exclude
// each other — a typed port is absolute, a shift applies to the one the file declares — and either one
// forces it, which makes it hold across reloads.
func load(read loader, path string, port, shift int, watch paths, in config.Input) (config.Form, *config.Config, error) {
	form, cfg, err := read(path, watch, in)
	switch {
	case err != nil || cfg == nil:
	case port > 0:
		cfg.Port, cfg.ForcedPort = port, port
	case shift != 0:
		cfg.Port, cfg.ForcedPort = cfg.Port+shift, cfg.Port+shift
	}
	return form, cfg, err
}

// portShift is the port shift of this instance, as its environment carries it: the service's unit sets
// it, and everything the service starts inherits it — sessions, terminal, update. It is the only reader
// of the variable; --port-shift is only its command-line form, and a value that is not an integer
// shifts nothing.
func portShift() int {
	shift, _ := strconv.Atoi(os.Getenv(shiftEnv))
	return shift
}

// shiftEnv carries the port shift to everything an instance starts.
const shiftEnv = "DSVS_PORT_SHIFT"

// role is what a process does with its life: the flag that asks for it, and what it runs. They all
// exclude each other.
type role struct {
	name  string
	asked bool
	run   func() error
}

// only returns the role asked for. Two role flags at once are refused by naming them, where the order
// of the lines would decide silently; no flag means byDefault.
func only(roles []role, byDefault func() error) (func() error, error) {
	chosen, asked := byDefault, []string(nil)
	for _, wanted := range roles {
		if wanted.asked {
			chosen, asked = wanted.run, append(asked, "--"+wanted.name)
		}
	}
	if len(asked) > 1 {
		return nil, fmt.Errorf("%s: one role at a time", strings.Join(asked, " "))
	}
	return chosen, nil
}

// printConfig writes the configuration these asked-for values build, like a session opened here on this
// network — defaults applied, routes sorted — in the JSON format it can read back: a snapshot, hence
// without the form a .json cannot declare. It refuses what a session would refuse. What the script said
// on its console goes to stderr: the output stays replayable as is. asForm writes the form instead,
// refused or not: it is the form that says what is hidden, inert or invalid, and the refusal follows.
func printConfig(out io.Writer, s *shape, pairs []string, observed *config.Observed, asForm bool) error {
	path, err := resolve(s.config)
	if err != nil {
		return err
	}
	values, err := valued("config", pairs)
	if err != nil {
		return err
	}
	form, cfg, err := load(config.Load, path, s.port, s.shift, s.watch, core.InputFor(path, observed, nil, values))
	if err != nil {
		return err
	}
	refused := form.Accept(values)
	if refused != nil {
		refused = fmt.Errorf("%s: %w", path, refused)
	}
	if asForm {
		return errors.Join(write(out, form), refused)
	}
	if refused != nil {
		return refused
	}
	for _, said := range cfg.Echo {
		fmt.Fprintln(os.Stderr, said)
	}
	// What is watched reads in a single place: the file's paths as well as the session's.
	cfg.Watch = cfg.Watched()
	text, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", text)
	return err
}

// printSession writes what the session here has in effect: the configuration its values built, or with
// asForm its form, where each value comes from included. These are the session's values: asking for
// others, or for another network, makes no sense here, and dsvs set changes them.
func printSession(out io.Writer, s *shape, pairs []string, observed string, asForm bool) error {
	if len(pairs) > 0 || observed != "" {
		return fmt.Errorf("--session writes the values of the session: it takes neither name=value nor --observed; %s set changes them", build.Name())
	}
	path, err := resolve(s.config)
	if err != nil {
		return err
	}
	if !hub.Answers(hub.SessionAddr(path)) {
		return fmt.Errorf("no session serves %s: %s config without --session writes what a new one would build, %[2]s start opens one", path, build.Name())
	}
	if asForm {
		link, err := attach.Watch(path)
		if err != nil {
			return err
		}
		defer link.Close()
		state := <-link.States()
		return write(out, config.Form{Fields: state.Params, Errors: state.Errors})
	}
	text, err := attach.Configuration(path)
	if err != nil {
		return err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, text, "", "  "); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", indented.Bytes())
	return err
}

// networkFor returns the network dsvs config answers for: the one observed here, or the one this JSON
// describes — the observed of dsvs state --json, - for standard input, null for no network. An unknown
// key is an error: a whole state given for its network is called out, instead of answering offline.
func networkFor(source string) (*config.Observed, error) {
	if source == "" {
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		observed, _ := observe(ctx, "", func(line string) { fmt.Fprintln(os.Stderr, line) })
		return observed, nil
	}
	in := os.Stdin
	if source != "-" {
		file, err := os.Open(source)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		in = file
	}
	described := json.NewDecoder(in)
	described.DisallowUnknownFields()
	var seen *config.Observed
	if err := described.Decode(&seen); err != nil {
		return nil, fmt.Errorf("--observed %s: %w", source, err)
	}
	return seen, nil
}

// join returns the network of the apps: the private network for the supervisor, loopback otherwise. A
// handover does not set it up, it finds it again: it lives in the kernel, not in the process.
func join(cfg config.Config, ctl, fallback string, taken *core.Handover) (core.Network, error) {
	if ctl == "" {
		return core.Loopback{Reason: fallback}, nil
	}
	if taken != nil {
		return netns.Rejoin(ctl, cfg.Prefix(), taken.IPs)
	}
	names := make([]string, 0, len(cfg.Apps))
	for _, app := range cfg.Apps {
		names = append(names, app.Name)
	}
	return netns.Join(ctl, names, cfg.Prefix())
}

// bind opens the session's socket, or takes over the one a handover carried across: it never stopped
// accepting, so no client finds the place empty and starts a second one.
func bind(path string, taken *core.Handover) (*attach.Server, error) {
	if taken != nil {
		return attach.Adopt(path, taken.Listen, taken.Lock)
	}
	return attach.Bind(path)
}
