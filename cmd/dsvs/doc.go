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
// Global flags shape the session here (--config, --port, --net, --watch) and are
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
// Bare dsvs ensures the session of the project here, writes the state of the session and returns. App
// names are no longer first-rank arguments: they
// follow a command, and an unknown first argument is an error that points to dsvs start.
//
// # The plumbing, which stays as flags
//
// What the binary addresses to itself is not a command, and the command help does not show it: --hub
// is the hub, which holds the ports for every session and serves nothing itself; --session holds the
// session without a terminal, for the clients that attach to it; --version writes the version. The
// hub, spawn and build.sh type them. The role is a single decision point: a flag names it, two are
// refused, and none lets the command line decide.
//
// Everything is in English: the visible output — messages, errors, TUI, panel, the help text — as well
// as the comments and the documentation of the repository.
//
// # The systemd box
//
// The session re-executes itself as a transient unit of the user's systemd session: everything lives
// in the unit's cgroup, and systemd kills what remains there when the main process dies — a kill -9 no
// longer orphans anything, not even what a runner like nx detaches behind it (measured, systemd 257).
// It carries KillMode=mixed and TimeoutStopSec=30: without them systemd mows down the dev servers at
// the same time as the supervisor that is stopping them, and stopping must belong to the session.
// Without a systemd session, the tool runs without a box and the log says so — the cgroup guarantee
// then falls, and a brutal stop can leave a runner orphaned.
package main
