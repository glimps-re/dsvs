package attach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"dsvs/internal/core"
	"dsvs/internal/fdpass"
	"dsvs/internal/hub"
	"dsvs/internal/ui/plain"
)

// grace is the longest a session that did not start stays listening to say why. Without it, it would
// die before the client that launched it read the reason, and only a silent wait would remain.
const grace = 5 * time.Second

// rendering bounds the wait for interfaces when the session stops. An interface that never returned
// must not make the session immortal: a badly restored terminal is better than a SIGTERM with no
// effect.
const rendering = 2 * time.Second

// maxTail bounds the log tail a session returns in one go. Beyond it, this is no longer reading the
// tail of anything: what is wanted is the stream, and that attaches.
const maxTail = 2000

// Server is a session's socket. Interfaces attach to it, and it is also the session's voice before it
// serves: a client that arrived too early waits there, and that is where it learns the session did not
// start.
type Server struct {
	sock *hub.Socket

	// settled closes when the session is there, or when it gave up — once, whatever happens: a waiting
	// client must always end up knowing. What follows is written before the close and read after it,
	// there is nothing to lock.
	once    sync.Once
	settled chan struct{}
	ctx     context.Context
	leave   context.CancelFunc
	session *core.Session
	stop    func()
	because error

	// hosts are the interfaces being rendered. Stopping waits for them: a client that took its terminal
	// back while the session still paints it would find it dirty.
	hosts sync.WaitGroup

	// watching are the attached interfaces, in their order of arrival: they are how a panel knows a TUI
	// is already watching, and how to bring it back on screen.
	seen     sync.Mutex
	watching []*watched
}

// watched is an attached interface and the socket used to talk to it.
type watched struct {
	Interface
	conn *net.UnixConn
}

// Bind opens the socket of the session of this configuration and accepts right away: whatever arrives
// while it starts waits its turn without having to know it came too early.
func Bind(config string) (*Server, error) {
	sock, err := hub.Bind(socket(config))
	if err != nil {
		return nil, err
	}
	return accept(sock), nil
}

func accept(sock *hub.Socket) *Server {
	s := &Server{sock: sock, settled: make(chan struct{})}
	go s.welcome()
	return s
}

// Ready hands over the session to serve: those who were waiting see their interface open. ctx is the
// process's, not the apps' — an interface must be able to watch the apps stop.
func (s *Server) Ready(ctx context.Context, session *core.Session, stop func()) {
	s.settle(func() {
		s.ctx, s.leave = context.WithCancel(ctx)
		s.session, s.stop = session, stop
	})
}

// Fail says the session will not serve, and stays long enough for someone to come and read it.
func (s *Server) Fail(err error) {
	s.settle(func() { s.because = err })
	time.Sleep(grace)
}

// Close hands back the lent terminals: the attached interfaces go away, and it waits for them to finish
// writing on them before returning.
func (s *Server) Close() error {
	s.settle(func() { s.because = errors.New("the session stopped before serving") })
	err := s.sock.Close()
	if s.leave != nil {
		s.leave()
	}
	rendered := make(chan struct{})
	go func() {
		s.hosts.Wait()
		close(rendered)
	}()
	select {
	case <-rendered:
	case <-time.After(rendering):
	}
	return err
}

func (s *Server) settle(with func()) {
	s.once.Do(func() {
		with()
		close(s.settled)
	})
}

func (s *Server) welcome() {
	for {
		conn, err := s.sock.Accept()
		if err != nil {
			return
		}
		s.hosts.Add(1)
		go func() {
			defer s.hosts.Done()
			s.host(conn.(*net.UnixConn))
		}()
	}
}

// host serves a client. Two kinds: an interface, which lends its terminal for the TUI to render on,
// and a driver, which reads the state and gives orders without showing anything. The first word says
// which.
func (s *Server) host(conn *net.UnixConn) {
	defer conn.Close()
	greeting, fds, err := fdpass.Recv(conn)
	if err != nil {
		return
	}
	fields := strings.Fields(greeting)
	switch {
	case len(fields) >= 6 && fields[0] == "attach":
		s.render(conn, fields, fds)
	case len(fields) == 1 && fields[0] == "watch":
		shut(fds)
		s.pilot(conn)
	case len(fields) >= 2 && fields[0] == "logs":
		shut(fds)
		s.tail(conn, number(fields[1], 50), fields[2:])
	default:
		shut(fds)
		fmt.Fprintf(conn, "err unreadable greeting: %q\n", greeting)
	}
}

// render lends the client's terminal to the interface it asks for, and leaves with it.
func (s *Server) render(conn *net.UnixConn, fields []string, fds []int) {
	in, out, err := pair(fds)
	if err != nil {
		fmt.Fprintf(conn, "err %v\n", err)
		return
	}
	defer in.Close()
	defer out.Close()

	ui := fields[1]

	<-s.settled
	if s.because != nil {
		fmt.Fprintf(conn, "err %v\n", s.because)
		return
	}
	if _, err := fmt.Fprintln(conn, "ok"); err != nil {
		return
	}

	watching, leave := context.WithCancel(s.ctx)
	defer leave()

	// The TUI is driven — a size that changes, and it returns on its own; streamed logs have nothing
	// to receive and stop with the client.
	var resize func(cols, rows int)
	render := func() error { plain.Run(watching, s.session, out); return nil }
	go s.orders(conn, resize, leave)

	seen := s.saw(Interface{Kind: ui, Window: fields[5]}, conn)
	defer s.forget(seen)
	s.session.Log.Addf("-- %s attached (pid %s, window %s)", ui, fields[4], fields[5])
	err = render()
	s.session.Log.Addf("-- %s detached", ui)
	// A canceled interface is one that was called back — the session stops, or the client leaves. That
	// is not an incident, and the client has no reason to exit with an error over it.
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(conn, "err %v\n", err)
		return
	}
	fmt.Fprintln(conn, "bye")
}

// pilot serves a client that drives without watching: it receives the session's state on every
// change, and gives orders. The state goes out on the socket as one line of JSON, which forces no one
// to agree on anything but names.
func (s *Server) pilot(conn *net.UnixConn) {
	<-s.settled
	if s.because != nil {
		// A driver's stream is framed by line ends: a reason that contains some would stop after the
		// first, and the path of a configuration without the fault it is blamed for says nothing.
		fmt.Fprintf(conn, "err %s\n", strings.Join(strings.Fields(s.because.Error()), " "))
		return
	}
	watching, leave := context.WithCancel(s.ctx)
	defer leave()
	go s.orders(conn, nil, leave)

	changed, unwatch := s.session.Watch()
	defer unwatch()
	for {
		text, err := json.Marshal(s.state())
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(conn, "state %s\n", text); err != nil {
			return
		}
		select {
		case <-watching.Done():
			return
		case <-changed:
		}
	}
}

// tail returns the tail of the logs: the session's, and those of the apps these names bring along —
// all of them with no name. It is a question, not an attachment: the answer goes out as one line of
// JSON, like a state, and nothing follows.
func (s *Server) tail(conn *net.UnixConn, n int, names []string) {
	<-s.settled
	if s.because != nil {
		fmt.Fprintf(conn, "err %s\n", strings.Join(strings.Fields(s.because.Error()), " "))
		return
	}
	wanted, err := s.wanted(names)
	if err != nil {
		fmt.Fprintf(conn, "err %s\n", strings.Join(strings.Fields(err.Error()), " "))
		return
	}
	series := [][]core.Line{ending(s.session.Log)}
	for _, app := range s.session.Apps() {
		if slices.Contains(wanted, app.Name) {
			series = append(series, ending(app.Log))
		}
	}
	merged := core.Merge(series...)
	if keep := min(n, maxTail); len(merged) > keep {
		merged = merged[len(merged)-keep:]
	}
	out := make([]Log, 0, len(merged))
	for _, line := range merged {
		out = append(out, Log{At: line.At, Src: line.Src, Text: line.Text})
	}
	text, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Fprintf(conn, "logs %s\n", text)
}

// saw records an attached interface, and returns what it takes to forget it when it leaves.
func (s *Server) saw(seen Interface, conn *net.UnixConn) *watched {
	s.seen.Lock()
	defer s.seen.Unlock()
	kept := &watched{Interface: seen, conn: conn}
	s.watching = append(s.watching, kept)
	return kept
}

func (s *Server) forget(gone *watched) {
	s.seen.Lock()
	defer s.seen.Unlock()
	s.watching = slices.DeleteFunc(s.watching, func(other *watched) bool { return other == gone })
}

func (s *Server) attached() []Interface {
	s.seen.Lock()
	defer s.seen.Unlock()
	out := make([]Interface, 0, len(s.watching))
	for _, seen := range s.watching {
		out = append(out, seen.Interface)
	}
	return out
}

// orders listens to what the client has to say while the interface runs. Its socket closing means
// detaching: the interface leaves with it, the session goes on.
func (s *Server) orders(conn *net.UnixConn, resize func(cols, rows int), leave func()) {
	defer leave()
	for {
		lines, _, err := fdpass.Recv(conn)
		if err != nil || lines == "" {
			return
		}
		// A stream socket does not keep write boundaries: two orders close together arrive together.
		for _, line := range strings.Split(lines, "\n") {
			verb, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
			switch {
			case verb == "size" && resize != nil:
				fields := strings.Fields(rest)
				if len(fields) == 2 {
					resize(number(fields[0], 80), number(fields[1], 24))
				}
			case verb == "start", verb == "stop", verb == "restart":
				s.apply(verb, strings.Fields(rest))
			case verb == "reload":
				// Reread the configuration, without restarting anything. What a saved file already
				// triggers on its own, for the imports no watched path covers.
				_ = s.session.Reload()
			case verb == "quit":
				// Close everything: the session stops, and the attached interfaces watch it do so.
				s.stop()
			}
		}
	}
}

// apply carries an order to the apps these names bring along — an app, a facade or a group, exactly
// like the command line of a new session: a client joining a session already alive has no reason to go
// about it differently.
func (s *Server) apply(verb string, names []string) {
	wanted, err := s.wanted(names)
	if err != nil {
		s.session.Log.Addf("!! %v", err)
		return
	}
	for _, name := range wanted {
		app := s.session.App(name)
		if app == nil {
			continue
		}
		switch verb {
		case "start":
			app.Start()
		case "stop":
			app.Stop()
		case "restart":
			app.Restart()
		}
	}
}

// wanted returns the apps these names bring along, and all of the session's when there are none: on
// this socket, an order or a question with no name applies to everything. It is the only way to say
// "all" in the protocol, and the only place that translates it — dsvs start --all therefore sends
// nothing more than an order with no name.
func (s *Server) wanted(names []string) ([]string, error) {
	if len(names) > 0 {
		return s.session.Config().Wanted(names)
	}
	apps := s.session.Apps()
	every := make([]string, 0, len(apps))
	for _, app := range apps {
		every = append(every, app.Name)
	}
	return every, nil
}

// ending returns a log up to the line its cursor writes on, that one included when it holds something:
// a progress bar midway is what one wants to see, the empty line a log waits on is not a line.
func ending(journal *core.Journal) []core.Line {
	lines := journal.Lines()
	if len(lines) > 0 && lines[len(lines)-1].Text == "" {
		return lines[:len(lines)-1]
	}
	return lines
}

// shut closes descriptors nobody wants: a driver has no terminal to lend, and whatever it sent anyway
// must not leak.
func shut(fds []int) {
	for _, fd := range fds {
		syscall.Close(fd)
	}
}

// pair turns the received pair of descriptors into the input and output of the lent terminal. An
// incomplete send is not a terminal: what arrived is closed here rather than leaked.
func pair(fds []int) (in, out *os.File, err error) {
	if len(fds) != 2 {
		shut(fds)
		return nil, nil, errors.New("the client did not lend its terminal")
	}
	return os.NewFile(uintptr(fds[0]), "attach-in"), os.NewFile(uintptr(fds[1]), "attach-out"), nil
}

func number(text string, fallback int) int {
	if value, err := strconv.Atoi(text); err == nil && value > 0 {
		return value
	}
	return fallback
}
