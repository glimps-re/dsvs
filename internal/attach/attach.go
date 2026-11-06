package attach

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"

	"dsvs/internal/fdpass"
	"dsvs/internal/hub"
)

// patience is the time a session is given to open its socket. It opens it before reading its
// configuration, before taking its port: this delay only covers its start and its re-executions, not
// what it has to do afterwards — that is waited for on the socket.
const patience = 5 * time.Second

// reloading is the word by which a session says it is moving to the binary on disk. Its socket does
// not close — it crosses the exec — and the interface starts again on the other side: the client
// reattaches instead of exiting with an error.
const reloading = "reloading"

// Attach lends this process's terminal to the session of this configuration and returns when the
// interface goes away. start launches the session when it is not there; names are the apps to start,
// ordered after attaching so that a session already alive hears them like a new one.
//
// A session moving to the binary on disk does not count as a departure: the client reattaches, and the
// requested apps have already been requested.
func Attach(config, ui string, names []string, start func() error) error {
	for {
		err := attach(config, ui, names, start)
		if !errors.Is(err, errReloading) {
			return err
		}
		names = nil
	}
}

// errReloading says the session is replacing itself with the binary on disk.
var errReloading = errors.New("the session is taking the binary on disk")

func attach(config, ui string, names []string, start func() error) error {
	conn, err := dial(socket(config), start)
	if err != nil {
		return err
	}
	defer conn.Close()

	defer keep(os.Stdin)()

	cols, rows := size()
	hello := fmt.Sprintf("attach %s %d %d %d %s %s", ui, cols, rows, os.Getpid(), window(), strings.Join(shown(), " "))
	if err := fdpass.Send(conn, hello, int(os.Stdin.Fd()), int(os.Stdout.Fd())); err != nil {
		return err
	}
	// Until the session has said "ok", it has not touched the terminal: its refusal is written where the
	// cursor stayed, and there is nothing to restore.
	if err := answer(conn); err != nil {
		return err
	}
	if len(names) > 0 {
		fmt.Fprintf(conn, "start %s\n", strings.Join(names, " "))
	}
	go resizes(conn)

	// Nothing is left but to wait: the interface writes to our terminal from the session. "bye" says it
	// handed it back itself; anything else says it left while holding it.
	if err := answer(conn); err != nil {
		fmt.Print(handback)
		return err
	}
	return nil
}

// handback restores the alternate screen and the cursor, which an interface that left without handing
// back would otherwise keep forever. Write it only after taking the terminal: on a VTE, leaving the
// alternate screen without having entered it restores a cursor position never saved, hence the origin.
const handback = "\x1b[?1049l\x1b[?25h"

// socket is where the session of this configuration listens. The name comes from hub, which holds the
// rule for every socket of the tool: the client and the session therefore compute it the same way.
func socket(config string) string { return hub.SessionAddr(config) }

// maxTailed bounds the log tail read in one go: the session renders at most maxTail lines, and a log
// line is whatever an app chose to write.
const maxTailed = 1 << 24

// maxState bounds a state line. A panel reads what a session says about itself, not its logs: beyond
// that, the dialogue is no longer the one we think it is.
const maxState = 1 << 20

// Link is a driver's link with a session: its state on every change, and orders to give it. It is what
// a panel consumes, where an interface lends a terminal.
type Link struct {
	conn   *net.UnixConn
	states chan State
}

// Watch opens the link with a running session. It is not launched if it is not there: a driver watches
// what runs, it does not decide to run things.
func Watch(config string) (*Link, error) {
	return pilot(config, func() error { return absent(config) })
}

// absent is what anyone looking for a session no process holds gets to read. One sentence for all who
// look for it without wanting to launch it.
func absent(config string) error { return fmt.Errorf("no session serves %s", config) }

// Ensure opens the link with the session of this configuration, launching it if it is not there.
func Ensure(config string, start func() error) (*Link, error) {
	return pilot(config, start)
}

func pilot(config string, start func() error) (*Link, error) {
	conn, err := dial(socket(config), start)
	if err != nil {
		return nil, err
	}
	link := &Link{conn: conn, states: make(chan State, 1)}
	if _, err := fmt.Fprintln(link.conn, "watch"); err != nil {
		link.conn.Close()
		return nil, err
	}
	lines := bufio.NewScanner(link.conn)
	lines.Buffer(make([]byte, 0, 4096), maxState)
	// The first state comes right away, or the session's reason for having none: opening the link might
	// as well fail there, rather than return a panel that will never show anything.
	first, err := state(lines)
	if err != nil {
		link.conn.Close()
		return nil, err
	}
	link.states <- first
	go link.follow(lines)
	return link, nil
}

// States returns the session's states. The latest evicts the one waiting — a panel shows what is, not
// the queue of what was — and the channel closes when the session goes away.
func (l *Link) States() <-chan State { return l.states }

// Order gives the session an order: start, stop or restart of what these names bring along, reload to
// make it reread its configuration, renew to make it take the binary on disk without letting go of its
// apps, or quit to close everything.
func (l *Link) Order(verb string, names ...string) error {
	_, err := fmt.Fprintf(l.conn, "%s %s\n", verb, strings.Join(names, " "))
	return err
}

func (l *Link) Close() error { return l.conn.Close() }

// Follow lends this terminal to the session's logs as they come: it is attaching without an interface,
// and nothing starts there. It is not launched if it is not there.
func Follow(config string) error {
	return Attach(config, "plain", nil, func() error { return absent(config) })
}

// Tail returns the tail of the session's logs: its own, and those of the apps these names bring along —
// all of them with no name. It is not launched if it is not there: it reads what runs, it does not
// decide to run things.
func Tail(config string, n int, names []string) ([]Log, error) {
	conn, err := dial(socket(config), func() error { return absent(config) })
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "logs %d %s\n", n, strings.Join(names, " ")); err != nil {
		return nil, err
	}
	lines := bufio.NewScanner(conn)
	lines.Buffer(make([]byte, 0, 4096), maxTailed)
	if !lines.Scan() {
		return nil, errors.Join(errors.New("session interrupted"), lines.Err())
	}
	verb, rest, _ := strings.Cut(lines.Text(), " ")
	if verb != "logs" {
		return nil, errors.New(strings.TrimPrefix(lines.Text(), "err "))
	}
	var out []Log
	if err := json.Unmarshal([]byte(rest), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (l *Link) follow(lines *bufio.Scanner) {
	defer close(l.states)
	for {
		next, err := state(lines)
		if err != nil {
			return
		}
		select {
		case <-l.states:
		default:
		}
		l.states <- next
	}
}

// state reads the session's next state line.
func state(lines *bufio.Scanner) (State, error) {
	if !lines.Scan() {
		if err := lines.Err(); err != nil {
			return State{}, err
		}
		return State{}, errors.New("session interrupted")
	}
	verb, rest, _ := strings.Cut(lines.Text(), " ")
	if verb != "state" {
		return State{}, errors.New(strings.TrimPrefix(lines.Text(), "err "))
	}
	var out State
	if err := json.Unmarshal([]byte(rest), &out); err != nil {
		return State{}, err
	}
	return out, nil
}

// answer reads the session's next news. "bye" says the interface returned control and the terminal
// with it; a closed socket says the session left without handing it back.
func answer(conn *net.UnixConn) error {
	line, _, err := fdpass.Recv(conn)
	switch {
	case err != nil:
		return fmt.Errorf("session interrupted: %w", err)
	case line == "":
		return errors.New("session interrupted")
	case strings.HasPrefix(line, "err "):
		return errors.New(strings.TrimPrefix(line, "err "))
	case line == reloading:
		return errReloading
	case line == "ok" || line == "bye":
		return nil
	}
	return fmt.Errorf("unexpected answer from the session: %q", line)
}

// dial reaches the session, launching it if its socket is not there. A socket left over from a dead
// session does not answer: it is the same case, and the session being born replaces it.
func dial(addr string, start func() error) (*net.UnixConn, error) {
	var started bool
	deadline := time.Now().Add(patience)
	for {
		conn, err := net.Dial("unix", addr)
		if err == nil {
			return conn.(*net.UnixConn), nil
		}
		if !started {
			if err := start(); err != nil {
				return nil, err
			}
			started = true
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the session did not open %s: %w", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// resizes tells the session that the window changed. The signal arrives here, not there: the terminal
// is not its own.
func resizes(conn *net.UnixConn) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	for range winch {
		cols, rows := size()
		if _, err := fmt.Fprintf(conn, "size %d %d\n", cols, rows); err != nil {
			return
		}
	}
}

// keep saves the terminal's state to hand it back as received: the terminal belongs to the client, the
// session is what puts it in raw mode, and it can die without having handed it back.
func keep(tty *os.File) func() {
	state, err := term.GetState(tty.Fd())
	if err != nil {
		return func() {}
	}
	return func() { term.Restore(tty.Fd(), state) }
}

func size() (cols, rows int) {
	cols, rows, err := term.GetSize(os.Stdout.Fd())
	if err != nil {
		return 80, 24
	}
	return cols, rows
}

// window is the terminal's X window, as the terminal tells it to what it launches. It is what the panel
// will need to raise the TUI; a dash when it is unknown.
func window() string {
	if id := os.Getenv("WINDOWID"); id != "" {
		return id
	}
	return "-"
}

// shown says what the terminal can render. The session inherited an environment that is not this one:
// without these variables, it would pick colors from someone else's terminal.
func shown() []string {
	var out []string
	for _, name := range []string{"TERM", "COLORTERM", "NO_COLOR", "CLICOLOR_FORCE"} {
		if value := os.Getenv(name); value != "" {
			out = append(out, name+"="+value)
		}
	}
	return out
}
