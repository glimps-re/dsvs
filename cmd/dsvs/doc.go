// Command dsvs runs the apps declared in config.dsvs.ts behind a single port.
//
// # The commands, and the table that holds them
//
// The command line is `dsvs [options] <command> [options] [arguments]`. commands.go holds the table:
// for each command, its name, the usage of its arguments, its summary, its long text, the flags it
// declares and what it runs. It is the only source — the general help, the help of a command and what
// gets run all come from it, and rendering is a pure function to an io.Writer so that the README and
// the skill use it without running the binary.
//
// Global flags shape the session here (--config, --port, --port-shift, --net, --watch, --tui) and are
// accepted before as well as after the command: a flag typed before is the default of the one after,
// which makes both places equivalent without anything having to choose between them.
//
// A flag written after the arguments is a flag, not a name: flag stops at the first word that is not a
// flag, and parse resumes parsing after each one. A -- keeps its meaning, and what follows it is
// literal.
//
// start, stop and restart take --all — every app of the configuration — instead of names, and not
// both. The client does not load the configuration: the session resolves "all", and --all adds nothing
// to the socket protocol, where an order without a name already covers everything.
//
// Bare dsvs ensures the service and the session of the project here, writes the state of the session
// and returns; --tui opens the interface on it. App names are no longer first-rank arguments: they
// follow a command, and an unknown first argument is an error that points to dsvs start.
//
// # The plumbing, which stays as flags
//
// What the binary addresses to itself is not a command, and the command help does not show it: --tray
// holds the status bar icon, and that is the role the systemd unit runs; --hub is the hub, which holds
// the ports for every session and serves nothing itself; --session holds the session without a
// terminal, for the clients that attach to it; --install-unit installs the unit; --version writes the
// version. The installed unit, the hub, spawn, build.sh and update.VersionOf type them — including an
// old binary during an update, which will never know the commands of the new one. The role is a single
// decision point: a flag names it, two are refused, and none lets the command line decide.
//
// The icon's socket is the voice of the service: it accepts quit and restart, whatever process sends
// them — that is the way dsvs service stop and restart go. Setting up an icon when another one is
// already there is not an error: there is nothing to do, and nothing to say.
//
// Everything is in English: the visible output — messages, errors, TUI, panel, the help text — as well
// as the comments and the documentation of the repository.
//
// # Two systemd units, not to be confused
//
// The session re-executes itself as a transient unit of the user's systemd session: everything lives
// in the unit's cgroup, and systemd kills what remains there when the main process dies — a kill -9 no
// longer orphans anything, not even what a runner like nx detaches behind it (measured, systemd 257).
// It carries KillMode=mixed and TimeoutStopSec=30: without them systemd mows down the dev servers at
// the same time as the supervisor that is stopping them, and stopping must belong to the session.
// Without a systemd session, the tool runs without a box and the log says so — the cgroup guarantee
// then falls, and a brutal stop can leave a runner orphaned.
//
// The installed unit is the service's, and has none of these directives: it runs --tray at session
// login, under WantedBy=default.target, and Restart=on-failure makes it retry, bounded, until the
// manager's environment carries DISPLAY. The details of the targets and the numbers are in unit.go,
// with what they cover.
//
// --install-unit installs it, called by build.sh --install as by install.sh: the binary carries the
// text of its unit, the scripts do not duplicate it. Writing the file is the contract; enabling it is
// best effort, and its failure outside a systemd session is reported without failing the installation.
// The unit also carries the settings of the instance, and the port shift is one of them:
// --install-unit --port-shift writes them there, and a reinstallation that asks for nothing — the one
// of every update — keeps those already there, so that the instance does not lose them when moving to
// the new binary.
//
// Installation also leaves the service running behind the unit, so that the icon is there without
// waiting for the next login: it asks for enable --now when there is a status bar and nobody holds the
// icon's socket yet, and its last sentence says which of the three cases happened.
package main
