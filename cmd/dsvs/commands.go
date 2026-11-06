package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"dsvs/internal/build"
	"dsvs/internal/config"
)

// command is a command of the command line, and the table holding them is the only source: the
// general help, the help of one command and what gets executed all come from it. declare sets the
// command's options on its FlagSet and returns what it executes — options are thus read where they
// are written, and there is nothing to read before the line is parsed.
type command struct {
	name    string
	args    string
	summary string
	long    string
	declare func(*flag.FlagSet) func(names []string) error
}

// commands is the table, in the order the help shows it: common usage first.
func commands(s *shape) []command {
	return []command{{
		name:    "start",
		args:    "[--all] <name...>",
		summary: "start the apps these names pull in",
		long: `start starts the apps these names pull in, in the session of the configuration found here. That
session is started if it is not running yet, and the background service with it. The state of
the session is written as soon as the order is placed and the prompt returns: the session lives
without a terminal, and keeps running. --all takes every app of the configuration instead of names.
--tui opens the interface on it instead of returning.`,
		declare: ordering(s, "start"),
	}, {
		name:    "stop",
		args:    "[--all] <name...>",
		summary: "stop the apps these names pull in",
		long: `stop stops the apps these names pull in, or every app of the configuration with --all. The
session keeps its port and its facades — a path leading to a stopped app answers 502 — and dsvs
quit is what closes it. A session must be running.`,
		declare: ordering(s, "stop"),
	}, {
		name:    "restart",
		args:    "[--all] <name...>",
		summary: "restart the apps these names pull in",
		long: `restart stops then starts the apps these names pull in, or every app of the configuration with
--all, each with the declaration in force — that is how an app whose command changed takes the new
one. A session must be running.`,
		declare: ordering(s, "restart"),
	}, {
		name:    "reload",
		summary: "make the session read its configuration again",
		long: `reload makes the session read its configuration again, without restarting anything: facades,
routes, proxy targets and groups follow the file. Saving the file does the same on its own. A
session must be running.`,
		declare: direct(reloading(s)),
	}, {
		name:    "quit",
		summary: "stop the apps of the session and close it",
		long: `quit stops the apps of the session, releases its port and closes it. The attached interfaces
watch it go. The background service and its icon stay: dsvs service stop is what sends them
away.`,
		declare: direct(closing(s)),
	}, {
		name:    "state",
		args:    "[--json]",
		summary: "write what the session here is doing",
		long: `state writes what the session of the configuration found here is doing: its apps, with what each
one is doing and since when, its facades, and the interfaces attached to it. It is what the
session pushes to whoever pilots it, and it does not start anything.`,
		declare: reading(func(asJSON bool) error { return showState(s, asJSON) }),
	}, {
		name:    "logs",
		args:    "[-n <lines>] [name...]",
		summary: "write the end of the logs, or follow them with -f",
		long: `logs writes the end of the logs of the session and of the apps these names pull in — all of
them when no name is given — and returns. -f follows them instead, until ctrl+c; it shows every
log, names or not. A session must be running.`,
		declare: func(fs *flag.FlagSet) func([]string) error {
			n := fs.Int("n", 50, "how many `lines` to write")
			follow := fs.Bool("f", false, "follow the logs as they are written, until ctrl+c")
			asJSON := fs.Bool("json", false, "write the lines as JSON, dated and attributed")
			return func(names []string) error { return showLogs(s, *n, *follow, *asJSON, names) }
		},
	}, {
		name:    "run",
		args:    "[name...]",
		summary: "hold the session in this terminal, streaming the logs",
		long: `run holds the session of the configuration found here in this terminal, streaming the logs
as they are written; ctrl+c stops everything. That is the shape of a script, where nothing must
survive the command. The apps these names pull in are started with it.`,
		declare: direct(func(names []string) error { return serve(false, s, names) }),
	}, {
		name:    "status",
		args:    "[--json]",
		summary: "write what runs on this machine, and who holds the ports",
		long: `status writes what runs: this binary and its version, its background service, the sessions it
holds a port for, and the other dsvs binaries with theirs. It is the answer to "what holds my
port", and it needs neither a configuration nor a service.`,
		declare: reading(func(asJSON bool) error { return status(asJSON) }),
	}, {
		name:    "service",
		args:    "[stop|restart]",
		summary: "start the background service and its icon, or stop it",
		long: `service starts the background service and its status bar icon, then returns; it is what the
installed systemd unit does at session start, and any command needing it does it on demand. stop
sends it away, icon included, and restart makes it take the binary on disk.`,
		declare: direct(service),
	}, {
		name:    "update",
		summary: "take a newer version of this binary, and restart the service",
		long: `update takes a newer version of this binary if there is one, hands the running sessions over to
it and restarts the service. Where it looks was decided when this binary was built: the clone it
came out of, or the published releases. A released binary is downloaded over HTTPS and checked
against the checksums of its release before it replaces anything; dsvs follows the stable
releases, dsvs-beta the pre-releases too. DSVS_RELEASES points at a mirror of the releases.`,
		declare: direct(func([]string) error { return upgrade() }),
	}, {
		name:    "panel",
		summary: "open the panel of the running sessions",
		long: `panel opens the panel of the projects and their sessions, without going through the status bar
icon, and returns when it closes.`,
		declare: direct(func([]string) error { return openPanel() }),
	}, {
		name:    "config",
		summary: "write the resolved configuration as JSON",
		long: `config writes the configuration as the tool sees it — defaults applied, routes ordered — in the
JSON format it can read back. What the configuration script says on its console goes to stderr,
so that the output stays replayable as it is.`,
		declare: direct(func([]string) error { return printConfig(s) }),
	}, {
		name:    "skill",
		summary: "write a skill that teaches this tool to a coding agent",
		long: `skill writes on stdout the skill a coding agent loads to know this tool: what it is, the list of
the commands, and which of them to ask for the rest. It comes out of the same table as the help,
so it cannot drift from it. Install it where the agent looks:

  dsvs skill > ~/.claude/skills/dsvs/SKILL.md`,
		declare: direct(func([]string) error { writeSkill(os.Stdout, s); return nil }),
	}, {
		name:    "help",
		args:    "[command]",
		summary: "write what a command does, or the list of them",
		long:    `help writes what a command does and the options it takes; without a name, the list of them.`,
		declare: direct(func(names []string) error { return helping(s, os.Stdout, names) }),
	}}
}

// dispatch executes the command of this line. Global options are already parsed: what remains starts
// with a command name, or with nothing — and an app name as first argument is an error that says
// which way to go, since app names are only written after a command now.
func dispatch(s *shape, args []string) error {
	if len(args) == 0 {
		return bare(s)
	}
	cmd, known := pick(s, args[0])
	if !known {
		return fmt.Errorf("unknown command %q; if it is an app, a facade or a group, start it:\n  %s start %s",
			args[0], build.Name(), strings.Join(args, " "))
	}
	fs := newFlagSet("dsvs " + cmd.name)
	s.declare(fs)
	run := cmd.declare(fs)
	names, err := parse(fs, args[1:])
	switch {
	case errors.Is(err, flag.ErrHelp):
		return describe(os.Stdout, cmd, s)
	case err != nil:
		return fmt.Errorf("%w (%s help %s)", err, build.Name(), cmd.name)
	}
	return run(names)
}

// general writes the help: the table as is, what to know about the tool, then the global options.
// It is a rendering and nothing else — the README and the skill read it back in memory.
func general(out io.Writer, s *shape) {
	fmt.Fprint(out, `dsvs — control panel for dev-servers

  dsvs [options] <command> [options] [arguments]

Commands:
`)
	list(out, s)
	fmt.Fprintf(out, "\n%s\nOptions, before or after the command:\n", intro)
	options(out, globals(s))
}

// describe writes what a command does and the options it takes. Global options are not there: they
// apply to every command, and the general help carries them.
func describe(out io.Writer, cmd command, s *shape) error {
	fmt.Fprintf(out, "  dsvs %s\n\n%s\n", cmd.usage(), cmd.long)
	fs := newFlagSet("dsvs " + cmd.name)
	cmd.declare(fs)
	var block bytes.Buffer
	options(&block, fs)
	if block.Len() == 0 {
		return nil
	}
	fmt.Fprint(out, "\nOptions:\n")
	_, err := block.WriteTo(out)
	return err
}

// helping is the help command: the general help, or the help of the named command.
func helping(s *shape, out io.Writer, names []string) error {
	if len(names) == 0 {
		general(out, s)
		return nil
	}
	cmd, known := pick(s, names[0])
	if !known {
		return fmt.Errorf("unknown command %q; %s help lists them", names[0], build.Name())
	}
	return describe(out, cmd, s)
}

// intro says what to know about the tool before its commands. The general help carries it and the
// skill repeats it: the same text for the human and for the agent, not two.
const intro = `A name is an app, a facade — which pulls in the apps of its routes — or a group declared in the
configuration. The session port is taken when it starts and released when it stops; every facade
answers on it, and a path leading to a stopped app answers 502.

dsvs needs a configuration: the one named by --config, or the first one found searching up from the
current directory. Everything it needs around it starts on demand and says so — the background
service and its icon, then the session, which lives without a terminal and survives the commands and
interfaces that watch it.

dsvs with no command starts what the configuration found here needs, writes the state of its session
and returns; --tui opens the interface on that session instead. A configuration runs only once, and
two sessions never fight over a host: the second one is refused, and says so.

A few flags are the binary talking to itself — --tray, --hub, --session, --install-unit, --version —
and are not commands.
`

// list writes the table: each command with the usage of its arguments and its summary, in columns.
// The general help and the skill both come from it.
func list(out io.Writer, s *shape) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, cmd := range commands(s) {
		fmt.Fprintf(w, "  %s\t%s\n", cmd.usage(), cmd.summary)
	}
	w.Flush()
}

// pick returns the command of this name: it is the only way to search the table.
func pick(s *shape, name string) (command, bool) {
	for _, cmd := range commands(s) {
		if cmd.name == name {
			return cmd, true
		}
	}
	return command{}, false
}

// usage is the command as typed, without the binary name: its name, and the usage of its arguments.
func (c command) usage() string { return strings.TrimSpace(c.name + " " + c.args) }

// newFlagSet returns a silent FlagSet: help and parse errors are written here, not in flag — one
// goes to stdout, the other to stderr with the matching exit code.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// options writes the options of a FlagSet as they are typed: two dashes for a long name, one for a
// letter.
func options(out io.Writer, fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		dashes := "--"
		if len(f.Name) == 1 {
			dashes = "-"
		}
		placeholder, usage := flag.UnquoteUsage(f)
		if placeholder != "" {
			placeholder = " <" + placeholder + ">"
		}
		fmt.Fprintf(out, "  %s%s%s\n    \t%s", dashes, f.Name, placeholder, usage)
		// An empty, zero or false default is not written: the absence of the option already says it.
		if f.DefValue != "" && f.DefValue != "0" && f.DefValue != "false" {
			fmt.Fprintf(out, " (default: %s)", f.DefValue)
		}
		fmt.Fprintln(out)
	})
}

// shape holds the global options: those that shape the session of here. They are accepted before as
// well as after the command, and an option typed before is the default of the one after — that is
// what makes both places equivalent without anything having to choose between them.
type shape struct {
	config string
	port   int
	shift  int
	net    string
	tui    bool
	watch  paths
}

// declare sets the global options on this FlagSet, with the values it already holds as defaults.
func (s *shape) declare(fs *flag.FlagSet) {
	fs.StringVar(&s.config, "config", s.config,
		"path of the configuration `file` (default: the first "+config.Names[0]+" found searching up)")
	fs.IntVar(&s.port, "port", s.port, "session `port`, instead of the one in the configuration")
	fs.IntVar(&s.shift, "port-shift", s.shift,
		"`offset` added to the port the configuration declares, so that a second binary can serve the same projects")
	fs.StringVar(&s.net, "net", s.net, "apps `network`: \"auto\", \"netns\" to require it, \"loopback\" to do without")
	fs.BoolVar(&s.tui, "tui", s.tui, "open the interface on the session, instead of returning")
	fs.Var(&s.watch, "watch",
		"`path` whose save makes the configuration reload, on top of those it declares; repeatable")
}

// globals returns a FlagSet holding only the global options, to write them in the help.
func globals(s *shape) *flag.FlagSet {
	fs := newFlagSet("dsvs")
	(&shape{net: s.net, shift: s.shift}).declare(fs)
	return fs
}

// args returns the options that shape a session, as passed on to the one being started.
func (s *shape) args() []string {
	var out []string
	if s.port > 0 {
		out = append(out, "--port", strconv.Itoa(s.port))
	}
	// The shift travels through the environment when the service unit sets it; typed by hand, this is
	// its only route to the session being started.
	if s.shift != 0 {
		out = append(out, "--port-shift", strconv.Itoa(s.shift))
	}
	if s.net != "auto" {
		out = append(out, "--net", s.net)
	}
	for _, path := range s.watch {
		out = append(out, "--watch", path)
	}
	return out
}

// paths collects a repeatable option. The plural is in the value, not in the grammar: --watch is
// repeated once per path.
type paths []string

func (p paths) String() string { return strings.Join(p, ", ") }

func (p *paths) Set(value string) error {
	*p = append(*p, value)
	return nil
}

// parse parses a line where options and arguments interleave, and returns the arguments. flag stops
// at the first word that is not an option: that word is stacked and parsing resumes after it, so an
// option written after a name stays an option. A -- keeps its meaning: what follows it is literal.
func parse(fs *flag.FlagSet, line []string) ([]string, error) {
	var args []string
	for len(line) > 0 {
		if line[0] == "--" {
			return append(args, line[1:]...), nil
		}
		if err := fs.Parse(line); err != nil {
			return nil, err
		}
		if line = fs.Args(); len(line) == 0 {
			break
		}
		args, line = append(args, line[0]), line[1:]
	}
	return args, nil
}

// ordering is an order about apps: names, or --all for every app of the configuration, not both.
// --all adds nothing to the socket protocol — an order without names already covers every app there,
// and the session resolves them, being the only one with the configuration at hand.
func ordering(s *shape, verb string) func(*flag.FlagSet) func([]string) error {
	return func(fs *flag.FlagSet) func([]string) error {
		var all bool
		fs.BoolVar(&all, "all", false, "every app of the configuration, instead of naming them")
		fs.BoolVar(&all, "a", false, "shorthand for --all")
		return func(names []string) error {
			switch {
			case all && len(names) > 0:
				return fmt.Errorf("%s takes --all or names, not both", verb)
			case !all && len(names) == 0:
				return fmt.Errorf("%s: name at least one app, facade or group, or --all for every app; %s state lists them", verb, build.Name())
			}
			return order(s, verb, names)
		}
	}
}

// direct is a command without options: the function that executes it, as is.
func direct(run func(names []string) error) func(*flag.FlagSet) func([]string) error {
	return func(*flag.FlagSet) func([]string) error { return run }
}

// reading is a command that only reads: --json is the only option it takes, the same everywhere, and
// it takes no argument — giving one is an error that says so, rather than a word nobody reads.
func reading(run func(asJSON bool) error) func(*flag.FlagSet) func([]string) error {
	return func(fs *flag.FlagSet) func([]string) error {
		asJSON := fs.Bool("json", false, "write the answer as JSON, for a script or an agent")
		return func(names []string) error {
			if len(names) > 0 {
				return fmt.Errorf("%s takes no argument, got %q", fs.Name(), strings.Join(names, " "))
			}
			return run(*asJSON)
		}
	}
}
