package hub

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"dsvs/internal/build"
	"dsvs/internal/fdpass"
)

// Version is that of the dialogue between a session and the hub. Two binaries that do not share it
// do not half-understand each other: the hub refuses the session, which says so and stops.
//
// The hello carries the requested port and the configuration that requests it, and a registered
// session can ask for another port.
const Version = 4

// anyHost is the host the session declares to say "everything nobody else claims". It is said,
// instead of saying nothing: a session without a facade and a session that takes the rest thus
// write two different lines, and the second is no longer mistaken for the silence of the first.
const anyHost = "*"

// Addr names the hub's socket. It lives in the user's session directory, a file path and not an
// abstract socket — the latter does not cross the boundary of a network namespace, where half the
// sessions live (measured: connection refused from a new namespace).
//
// Its name comes from the executable's: two binaries with different names never meet, and one
// therefore does not refuse the other's sessions at the version check.
func Addr() string {
	return filepath.Join(runtimeDir(), exeName()+"-hub.sock")
}

// SessionAddr names a session's socket, through which the interfaces attach to it. It comes from
// its configuration's path, which is its identity: whoever knows which configuration they want
// knows where to knock, with no directory to consult. The digest rather than the path, because a
// socket name carries no slash and is bounded.
func SessionAddr(config string) string {
	sum := sha256.Sum256([]byte(config))
	return filepath.Join(runtimeDir(), fmt.Sprintf("%s-%x.sock", exeName(), sum[:8]))
}

// TrayAddr names the status bar icon's socket. It only serves to ensure there is just one: taking
// it fails when another one holds it.
func TrayAddr() string {
	return filepath.Join(runtimeDir(), exeName()+"-tray.sock")
}

// Answers says whether someone answers behind this socket. A file left over by a dead process does
// not answer: it is the only question that matters.
func Answers(addr string) bool {
	conn, err := net.Dial("unix", addr)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Awaits waits for someone to answer behind this socket: a process just born, or that just exec'd,
// takes a moment to take it back.
func Awaits(addr string) bool {
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if Answers(addr) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func runtimeDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "dsvs-"+strconv.Itoa(os.Getuid()))
	}
	return dir
}

// Session is a session registered with the hub, as the hub knows it: its port and its
// configuration, which is its identity.
type Session struct {
	Port   int    `json:"port"`
	Config string `json:"config"`
}

// Sessions tells the living sessions. No hub, no sessions: it is not an error, it is the answer —
// and none is launched to ask the question.
func Sessions() ([]Session, error) {
	out, err := sessions(Addr())
	if _, dead := err.(*net.OpError); dead {
		return nil, nil
	}
	return out, err
}

// sessions asks a hub for its list, ours or another binary's. A hub that does not answer is an
// error here: it is up to the caller to decide whether it is one for them.
func sessions(addr string) ([]Session, error) {
	conn, err := net.Dial("unix", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, "sessions"); err != nil {
		return nil, err
	}
	var out []Session
	lines := bufio.NewScanner(conn)
	for lines.Scan() {
		fields := strings.SplitN(lines.Text(), " ", 3)
		if len(fields) != 3 || fields[0] != "session" {
			continue
		}
		port, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		out = append(out, Session{Port: port, Config: fields[2]})
	}
	return out, lines.Err()
}

// Peer is another binary's hub: its name, what it says of its version and of its birth, and the
// sessions it holds. Version and Since are empty when it is too old to answer the question — what
// is known of it does not become false for all that.
type Peer struct {
	Name     string    `json:"name"`
	Version  string    `json:"version,omitempty"`
	Since    time.Time `json:"since,omitzero"`
	Sessions []Session `json:"sessions"`
}

// MarshalJSON writes its sessions, none included.
func (p Peer) MarshalJSON() ([]byte, error) {
	type plain Peer
	p.Sessions = append([]Session{}, p.Sessions...)
	return json.Marshal(plain(p))
}

// String names another binary with what it was willing to say of itself: its version, since when it
// has held the ports. A hub too old to answer does not make the line false, only shorter.
func (p Peer) String() string {
	var said []string
	if p.Version != "" {
		said = append(said, p.Version)
	}
	if !p.Since.IsZero() {
		said = append(said, "holding ports since "+p.Since.Format("15:04"))
	}
	if len(said) == 0 {
		return p.Name
	}
	return p.Name + " (" + strings.Join(said, ", ") + ")"
}

// Peers tells the other binaries' hubs. They never talk to each other, by design: they are looked
// at to name them — a taken port is taken by one of them, and saying so is better than letting one
// read "address already in use".
func Peers() []Peer {
	names, err := filepath.Glob(filepath.Join(runtimeDir(), "*-hub.sock"))
	if err != nil {
		return nil
	}
	var out []Peer
	for _, addr := range names {
		if addr == Addr() {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(addr), "-hub.sock")
		sessions, err := sessions(addr)
		if err != nil {
			continue // a file a dead hub left behind: nobody behind it, nothing to say
		}
		peer := Peer{Name: name, Sessions: sessions}
		peer.Version, peer.Since = who(addr)
		out = append(out, peer)
	}
	return out
}

// Holder tells who holds this port, among the other binaries: the hub and the session it opened it
// for. A port taken by any other program has no holder to name here.
func Holder(port int) (Peer, Session, bool) {
	for _, peer := range Peers() {
		for _, session := range peer.Sessions {
			if session.Port == port {
				return peer, session, true
			}
		}
	}
	return Peer{}, Session{}, false
}

// held names what holds this port, when another dsvs binary opened it: it is the only thing a taken
// port lacks for one to know what to do. A port taken by anything else has nothing to add.
func held(port int) string {
	peer, session, ok := Holder(port)
	if !ok {
		return ""
	}
	return fmt.Sprintf("; %s holds it for %s", peer, session.Config)
}

// who asks a hub for its version and its birth. Without an answer, it is not known, and that is
// all.
func who(addr string) (string, time.Time) {
	conn, err := net.Dial("unix", addr)
	if err != nil {
		return "", time.Time{}
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, "who"); err != nil {
		return "", time.Time{}
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", time.Time{}
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != "who" {
		return "", time.Time{}
	}
	since, _ := time.Parse(time.RFC3339, fields[2])
	return fields[1], since
}

// Front is the session's port, held by the hub. config is the session's identity: its configuration
// file, and nothing else. isolated says the session lives in a network namespace: the hub must be
// born outside. The port is the only place where the session keeps it: it registers on it, changes
// it, and registers again on it.
//
// The registration has three states, and nothing represents them outside the methods that follow:
// not taken yet, held by a hub's connection, or released for good. A hub that dies sends the
// session back through "not taken yet", from which it registers again; a released session never
// leaves that state, and no dead connection therefore passes for alive.
type Front struct {
	config   string
	isolated bool

	// hosts is the last announced list, kept for two reasons: it precedes the connection at start,
	// and it is sent whole on each registration.
	// send serializes the declarations among themselves, outside the state lock: what is written to
	// the hub no longer freezes the port loop nor the session's close.
	// moved receives the hub's answer to a port change: it is the listener loop that reads it, the
	// only one reading what the hub says. conns are the served connections, which the session
	// closes when it leaves the port they come from.
	send   sync.Mutex
	mu     sync.Mutex
	say    func(string)
	in     *fdpass.Reader
	port   int
	hosts  []string
	closed bool
	moved  chan string
	conns  map[*conn]struct{}
}

// New puts the hub in front of the session's port.
func New(config string, port int, isolated bool) *Front {
	return &Front{config: config, port: port, isolated: isolated, say: func(string) {},
		moved: make(chan string, 1), conns: map[*conn]struct{}{}}
}

// Say gives where to write what there is to say about the hub. The session's log does not exist yet
// when it is wrapped, but it is there before the port opens.
func (f *Front) Say(say func(string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.say = say
}

// Hosts tells the hub the hosts this session serves, the whole list every time: it is a
// declaration, not a difference to apply. Before the port opens, it is simply kept.
func (f *Front) Hosts(hosts []string) error {
	f.mu.Lock()
	f.hosts = named(hosts)
	f.mu.Unlock()
	return f.declare()
}

// declarePatience is the time the hub is given to swallow a declaration. Beyond it, the hub no
// longer reads: the line may be half sent, and a truncated line would make it serve hosts that were
// not declared. This registration is therefore dropped, and the port loop takes up a new one.
const declarePatience = 2 * time.Second

// declare tells the hub the current list. Without acknowledgment: until it arrives, the old one
// holds, served by the same session.
func (f *Front) declare() error {
	f.send.Lock()
	defer f.send.Unlock()
	f.mu.Lock()
	in, hosts := f.in, f.hosts
	f.mu.Unlock()
	if in == nil {
		return nil
	}
	conn := in.Conn()
	timed := conn.SetWriteDeadline(time.Now().Add(declarePatience))
	_, wrote := fmt.Fprintf(conn, "hosts %s\n", strings.Join(hosts, ","))
	if err := errors.Join(timed, wrote); err != nil {
		f.drop(in)
		return err
	}
	return nil
}

// drop drops this registration: the session goes back through "not taken yet", from which the port
// loop registers again. A reader the revival already replaced is no longer ours, and it is not
// dropped a second time.
func (f *Front) drop(in *fdpass.Reader) {
	f.mu.Lock()
	if f.in == in {
		f.in = nil
	}
	f.mu.Unlock()
	in.Close()
}

// Listen returns the port's connections, without taking it: the hub accepts them and passes them
// on. The port therefore belongs to nobody, which is what lets two sessions serve their respective
// hosts at the same time.
func (f *Front) Listen() (net.Listener, error) {
	port, born, err := f.take()
	if err != nil {
		return nil, fmt.Errorf("%w%s", err, held(port))
	}
	if born {
		f.tell(fmt.Sprintf("-- hub for port %d started", port))
	}
	return &listener{front: f}, nil
}

// Move switches the session to another port. The hub opens it if it is not open, leads its hosts
// there and releases the old one if nobody else serves on it, under its registrations' lock:
// refused — the port taken by another program, a host another session serves there —, the session
// stays where it is, and the error says why. The connections from the old port close: a tab that
// kept its own there is no longer served. A session that is not registered will register on the new
// one.
func (f *Front) Move(port int) error {
	f.send.Lock()
	defer f.send.Unlock()
	f.mu.Lock()
	in, left := f.in, f.port
	if in == nil {
		f.port = port
		f.mu.Unlock()
		return nil
	}
	f.mu.Unlock()
	// An answer that arrived too late for the previous change does not answer this one.
	select {
	case <-f.moved:
	default:
	}
	control := in.Conn()
	timed := control.SetWriteDeadline(time.Now().Add(declarePatience))
	_, wrote := fmt.Fprintf(control, "port %d\n", port)
	if err := errors.Join(timed, wrote); err != nil {
		f.drop(in)
		return err
	}
	select {
	case answer := <-f.moved:
		if answer != "ok" {
			return fmt.Errorf("%s%s", answer, held(port))
		}
	case <-time.After(declarePatience):
		f.drop(in)
		return fmt.Errorf("the hub did not answer")
	}
	f.mu.Lock()
	f.port = port
	var stale []*conn
	for served := range f.conns {
		if from, _ := served.LocalAddr().(*net.TCPAddr); from != nil && from.Port == left {
			stale = append(stale, served)
		}
	}
	f.mu.Unlock()
	for _, served := range stale {
		served.Close()
	}
	return nil
}

// take registers the session with its port's hub, launching it if there is none, and keeps the
// connection: it is through it that the port's connections then arrive. It returns that port, and
// says whether the hub had to be launched. The hosts can change while registering: what holds is
// the current list, told again to the one that just welcomed us with the old one.
func (f *Front) take() (int, bool, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return 0, false, net.ErrClosed
	}
	previous, port, announced := f.in, f.port, f.hosts
	f.in = nil
	f.mu.Unlock()
	if previous != nil {
		previous.Close()
	}

	in, born, err := join(port, f.config, announced, f.isolated, f.abandoned)
	if err != nil {
		return port, born, err
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		in.Close()
		return port, born, net.ErrClosed
	}
	f.in = in
	stale := !slices.Equal(announced, f.hosts)
	f.mu.Unlock()
	if stale {
		return port, born, f.declare()
	}
	return port, born, nil
}

// abandoned says the session released its port: whatever looks for a hub for it stops there, and
// the arriving hub never sees an already departed session register — it would believe it serves it,
// then die seeing it leave.
func (f *Front) abandoned() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// reader returns how to read the current hub: a re-registration replaces it, and the listener loop
// does not have to know which one it reads. Without a reader, there is nothing to read — the
// session is closed for good, which the second return value says, or it is no longer registered and
// its port is taken again.
func (f *Front) reader() (*fdpass.Reader, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.in, f.closed
}

// close releases the port for good: the session no longer registers again, including if the hub
// dies between the announcement of its death and the revival.
func (f *Front) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.in
	f.in, f.closed = nil, true
	if in == nil {
		return nil
	}
	return in.Close()
}

func (f *Front) tell(line string) {
	f.mu.Lock()
	say := f.say
	f.mu.Unlock()
	say(line)
}

// join joins the port's hub, launching it if it does not exist yet. The second return value says
// whether it had to be launched. A few rounds: the one just launched takes a moment to listen, and
// the one dying just as we arrive still accepts, then cuts — its death is not a refusal, and the
// next one is awaited rather than making the port silent for good.
func join(port int, config string, hosts []string, isolated bool, abandoned func() bool) (*fdpass.Reader, bool, error) {
	var born bool
	deadline := time.Now().Add(3 * time.Second)
	for {
		if abandoned() {
			return nil, born, net.ErrClosed
		}
		in, err := knock(port, config, hosts)
		var refused refusal
		switch {
		case err == nil:
			return in, born, nil
		case errors.As(err, &refused):
			return nil, born, refused
		}
		if !born {
			if err := spawnHub(isolated); err != nil {
				return nil, false, err
			}
			born = true
		}
		if time.Now().After(deadline) {
			return nil, born, fmt.Errorf("hub for port %d unreachable: %w", port, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// knock opens the hub's door and introduces itself: it returns how to read it, a refusal, or the
// reason there is nobody behind it.
func knock(port int, config string, hosts []string) (*fdpass.Reader, error) {
	conn, err := net.Dial("unix", Addr())
	if err != nil {
		return nil, err
	}
	in := fdpass.NewReader(conn.(*net.UnixConn))
	if err := hello(in, port, config, hosts); err != nil {
		in.Close()
		return nil, err
	}
	return in, nil
}

// hello tells the version spoken, the port requested, the hosts served and which configuration it
// comes from, in one piece: the hub's answer then means "registered, port taken, and I know for
// whom" — there is no instant where the session would be known without its hosts, and where a
// request for them would get a 503. The path comes last: it alone can carry a space.
func hello(in *fdpass.Reader, port int, config string, hosts []string) error {
	if _, err := fmt.Fprintf(in.Conn(), "hello %d %d %s %s\n", Version, port, strings.Join(hosts, ","), config); err != nil {
		return err
	}
	answer, err := in.Line()
	switch {
	case errors.Is(err, io.EOF):
		return fmt.Errorf("the hub for port %d stopped without answering", port)
	case err != nil:
		return err
	case answer != "ok":
		return refusal{strings.TrimPrefix(answer, "err ")}
	}
	return nil
}

// refusal is what the hub answers when it does not want the session — the port taken elsewhere, a
// version it does not speak: a decision, which no retry will lift. Everything else is an accident
// of the dialogue, and is waited out.
type refusal struct{ said string }

func (r refusal) Error() string { return r.said }

// named translates the configuration's hosts into the hub's vocabulary: a facade without a host
// takes what nobody claims, and says so by its name.
func named(hosts []string) []string {
	named := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if host == "" {
			host = anyHost
		}
		named = append(named, host)
	}
	return named
}

// spawnHub launches the hub. It is the only place where the session leaves its process, and
// therefore the only seam the test replaces to give itself a hub it can kill.
var spawnHub = spawn

// spawn launches the hub outside the session: a transient systemd unit, so that it outlives the one
// that launched it and does not die with its box. It is also what makes it be born in the normal
// world when the session lives in a network namespace — the manager is not in it. Without systemd,
// a mere detach is enough where there is no namespace to leave.
func spawn(isolated bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if runner, err := exec.LookPath("systemd-run"); err == nil {
		unit := exec.Command(runner, "--user", "--quiet", "--collect",
			"--description="+exeName()+" — port hub",
			"--setenv=XDG_RUNTIME_DIR="+os.Getenv("XDG_RUNTIME_DIR"),
			"--", exe, "--hub")
		if out, err := unit.CombinedOutput(); err == nil {
			return nil
		} else if !strings.Contains(string(out), "Failed to connect") {
			// A missing systemd session is an expected case, the rest is a real failure.
			return fmt.Errorf("hub: %v %s", err, strings.TrimSpace(string(out)))
		}
	}
	// A detach only leaves the session, never its network namespace: the hub would take a port
	// there that the machine does not see, and nothing would say so. Better refuse than serve into
	// the void — it is systemd that makes it be born outside.
	if isolated {
		return errors.New("the hub needs a systemd session, or --net loopback")
	}
	loose := exec.Command(exe, "--hub")
	loose.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return loose.Start()
}

// exeName is the name of the running binary, a hub's as well as a session's: both sides compute it
// the same way, and that is what makes them find each other.
func exeName() string { return build.Name() }

// listener presents the connections passed by the hub as those of an ordinary port: http.Server
// does not tell the difference.
type listener struct {
	front *Front
	// revived dates the last revival. A hub that accepts then dies at once, in a loop, therefore
	// does not make the revival spin faster than reviveEvery — neither the loop, nor the log.
	revived time.Time
}

// reviveEvery is the smallest interval between two revivals. The first does not wait for it: an
// isolated death is caught at once, and it is persistence that pays.
const reviveEvery = time.Second

// Accept waits for the next connection the hub hands us.
//
// A hub that dies does not take the session with it: its services keep running, and the port comes
// back to them as soon as a new hub is there — the session registers with it again rather than
// serving into the void. Its death reads as a stream that ends, or as a cut connection when it
// still had something to read (measured: ECONNRESET): any read that fails without being our own
// close says that this hub is no longer readable, and there is nothing to get from it but a new
// registration. What ends the server loop, exactly like a closed port, is our close, or a refusal
// that cannot be lifted alone: the port taken elsewhere, a version not spoken.
func (l *listener) Accept() (net.Conn, error) {
	for {
		in, err := l.reading()
		if err != nil {
			return nil, err
		}
		line, err := in.Line()
		if err != nil {
			// This reader is no longer readable: it is dropped, and the next round registers again.
			l.front.drop(in)
			continue
		}
		verb, rest, _ := strings.Cut(line, " ")
		switch verb {
		case "say":
			l.front.tell(rest)
			continue
		case "port":
			select {
			case l.front.moved <- rest:
			default:
			}
			continue
		case "conn":
		default:
			return nil, errors.New(strings.TrimPrefix(line, "err "))
		}
		fd, passed := in.Fd()
		if !passed {
			return nil, errors.New("the hub passed a connection without its descriptor")
		}
		file := os.NewFile(uintptr(fd), "hub")
		raw, err := net.FileConn(file)
		file.Close()
		if err == nil {
			return l.front.hold(raw), nil
		}
	}
}

// reading returns how to read the hub, taking the port again if the session is no longer registered
// there. Only its close for good, or a refusal that cannot be lifted alone, ends the loop.
func (l *listener) reading() (*fdpass.Reader, error) {
	for {
		in, closed := l.front.reader()
		switch {
		case closed:
			return nil, net.ErrClosed
		case in != nil:
			return in, nil
		}
		if wait := reviveEvery - time.Since(l.revived); wait > 0 {
			time.Sleep(wait)
		}
		l.revived = time.Now()
		port, _, err := l.front.take()
		if err != nil {
			return nil, err
		}
		l.front.tell(fmt.Sprintf("-- hub for port %d taken again, the port serves once more", port))
	}
}

// Close unregisters the session: the hub loses its last client and leaves with it.
func (l *listener) Close() error { return l.front.close() }

func (l *listener) Addr() net.Addr {
	l.front.mu.Lock()
	defer l.front.mu.Unlock()
	return &net.TCPAddr{Port: l.front.port}
}

// conn is a served connection, which the session closes if it leaves the port it comes from.
type conn struct {
	net.Conn
	front *Front
}

// hold keeps a served connection, until the server closes it.
func (f *Front) hold(raw net.Conn) net.Conn {
	served := &conn{Conn: raw, front: f}
	f.mu.Lock()
	f.conns[served] = struct{}{}
	f.mu.Unlock()
	return served
}

func (c *conn) Close() error {
	c.front.mu.Lock()
	delete(c.front.conns, c)
	c.front.mu.Unlock()
	return c.Conn.Close()
}
