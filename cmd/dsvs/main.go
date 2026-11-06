package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/x/term"

	"dsvs/internal/attach"
	"dsvs/internal/build"
	"dsvs/internal/config"
	"dsvs/internal/core"
	"dsvs/internal/gateway"
	"dsvs/internal/hub"
	"dsvs/internal/netns"
	"dsvs/internal/ui/plain"
)

func main() {
	s := &shape{net: "auto"}
	fs := newFlagSet("dsvs")
	s.declare(fs)
	// The plumbing the binary addresses to itself stays as flags, and is not a command: the hub, spawn
	// and build.sh type them.
	ports := fs.Bool("hub", false, "hold the ports for the sessions, without serving anything")
	headless := fs.Bool("session", false, "hold the session without an interface, for the clients that attach to it")
	tell := fs.Bool("version", false, "write the version of the binary and quit")
	switch err := fs.Parse(os.Args[1:]); {
	case errors.Is(err, flag.ErrHelp):
		general(os.Stdout, s)
		return
	case err != nil:
		die(fmt.Errorf("%w (%s help)", err, build.Name()))
	}

	// The role is a single decision point: one flag names it, two are refused, and none lets the
	// command line decide.
	chosen, err := only([]role{
		{"version", *tell, func() error { fmt.Println(build.Version); return nil }},
		{"hub", *ports, hub.Serve},
		{"session", *headless, func() error { return serve(true, s, fs.Args()) }},
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
	case inside:
	case s.net == "loopback":
		fallback = "asked for by hand"
	default:
		code, err := netns.Gate()
		switch {
		case err == nil && code != netns.Unreachable:
			os.Exit(code)
		case s.net == "netns":
			return fmt.Errorf("private network impossible: %w", cause(err))
		default:
			fallback = cause(err).Error()
		}
	}
	return run(s, headless, ctl, fallback, names)
}

// machine is what runs here: this binary, the sessions whose port it holds, and the other
// dsvs binaries with theirs. A single read, two renderings — the text and the JSON say the same thing.
type machine struct {
	Binary   string        `json:"binary"`
	Version  string        `json:"version"`
	Sessions []hub.Session `json:"sessions,omitempty"`
	Peers    []hub.Peer    `json:"peers,omitempty"`
}

// status writes what runs. It is the answer to "what holds my ports", and it needs no
// configuration.
func status(asJSON bool) error {
	sessions, err := hub.Sessions()
	if err != nil {
		return err
	}
	here := machine{
		Binary:   build.Name(),
		Version:  build.Version,
		Sessions: sessions,
		Peers:    hub.Peers(),
	}
	if asJSON {
		return write(os.Stdout, here)
	}
	writeStatus(os.Stdout, here)
	return nil
}

// writeStatus writes what runs for a human. It is a rendering and nothing else.
func writeStatus(out io.Writer, here machine) {
	fmt.Fprintf(out, "%s %s\n", here.Binary, here.Version)
	if len(here.Sessions) == 0 {
		fmt.Fprintln(out, "  sessions: none, no port held")
	}
	for _, session := range here.Sessions {
		fmt.Fprintf(out, "  port %d  %s\n", session.Port, session.Config)
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

// lend lends the terminal of this process to the session of the configuration here, starting it if it
// is not there yet. names are the apps to start, ordered after attaching.
func lend(s *shape, names []string) error {
	config, err := resolve(s.config)
	if err != nil {
		return err
	}
	return attach.Attach(config, ui(), names, func() error { return spawn(config, s.args()) })
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
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func run(s *shape, headless bool, ctl, fallback string, names []string) error {
	path, err := resolve(s.config)
	if err != nil {
		return err
	}

	// The session's socket opens before anything is read: it is named by the configuration, which is
	// the identity of the session, and it is through it that a client will learn the session did not
	// start — otherwise it would only have a silent wait.
	var attachers *attach.Server
	if headless {
		server, err := attach.Bind(path)
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
		if attachers != nil {
			attachers.Fail(err)
		}
		return err
	}

	cfg, err := load(path, s.port, s.watch)
	if err != nil {
		return failed(err)
	}
	names, err = cfg.Wanted(names)
	if err != nil {
		return failed(fmt.Errorf("%s: %w", cfg.Path, err))
	}

	network, err := join(cfg, ctl, fallback)
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
	session := core.NewSession(cfg, network, front, func(s *core.Session) core.Gateway {
		return gateway.NewProxy(cfg.Port, s, s.Sink())
	})

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

	serving, stop := context.WithCancel(context.Background())
	defer stop()

	// The port is taken here, before any interface: when busy, it is an error one reads, not a log line
	// in an interface that pretends to work. It is changed with --port.
	if err := session.Open(serving); err != nil {
		return failed(fmt.Errorf("port %d: %w%s", cfg.Port, err, held(cfg.Port)))
	}

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

// load reads the configuration and applies the session's choices: the port, and the paths to watch on
// top of its own.
func load(path string, port int, watch paths) (config.Config, error) {
	cfg, err := config.Load(path, watch)
	if err != nil {
		return config.Config{}, err
	}
	if port > 0 {
		// A choice of the session: it wins over the file at every read, reloads included.
		cfg.Port, cfg.ForcedPort = port, port
	}
	return cfg, nil
}

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

// printConfig writes the configuration as the tool sees it — defaults applied, routes sorted — in the
// JSON format it can read back. What the script said on its console goes to stderr: the output stays
// replayable as is.
func printConfig(s *shape) error {
	path, err := resolve(s.config)
	if err != nil {
		return err
	}
	cfg, err := load(path, s.port, s.watch)
	if err != nil {
		return err
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
	_, err = fmt.Printf("%s\n", text)
	return err
}

// join returns the network of the apps: the private network for the supervisor, loopback otherwise.
func join(cfg config.Config, ctl, fallback string) (core.Network, error) {
	if ctl == "" {
		return core.Loopback{Reason: fallback}, nil
	}
	names := make([]string, 0, len(cfg.Apps))
	for _, app := range cfg.Apps {
		names = append(names, app.Name)
	}
	return netns.Join(ctl, names, cfg.Prefix())
}
