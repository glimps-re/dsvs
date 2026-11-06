package hub

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"dsvs/internal/build"
	"dsvs/internal/fdpass"
)

// grace is the time a newborn hub grants the first session. With nobody needing it, it leaves: a
// hub never outlives what it serves.
const grace = 10 * time.Second

// Serve plays the hub's role: it opens the ports the sessions claim, keeps each one's list of hosts
// and passes them the connections that are theirs. It returns when the last session leaves, or
// right away if another hub is already there.
//
// A port held by something else is no longer news to announce to everyone: it is taken for the
// session that asks for it, and the refusal goes to that session alone.
func Serve() error {
	sock, err := Bind(Addr())
	switch {
	case errors.Is(err, ErrTaken):
		return nil // another one already is: nothing to do, and nothing to say
	case err != nil:
		return err
	}
	defer sock.Close()

	h := &hub{ports: map[int]net.Listener{}, changed: make(chan int, 8), born: time.Now()}
	go h.welcome(sock)
	h.live()
	return nil
}

// ErrTaken says someone already answers behind this socket: it is theirs to serve, not ours.
var ErrTaken = errors.New("already there")

// Bind opens one of the tool's sockets — the hub's, a session's — under a file lock the process
// keeps for its whole life: the right to hold that name is this lock. Only one process at a time
// can therefore delete the file left by a dead one and take its place, and nobody can delete a
// living one's socket — the kernel releases the lock with the last descriptor, sudden death
// included.
func Bind(addr string) (*Socket, error) {
	if err := os.MkdirAll(filepath.Dir(addr), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(addr+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return nil, err
		}
		return nil, ErrTaken
	}
	if err := os.Remove(addr); err != nil && !errors.Is(err, os.ErrNotExist) {
		lock.Close()
		return nil, err
	}
	sock, err := net.ListenUnix("unix", &net.UnixAddr{Name: addr, Net: "unix"})
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &Socket{UnixListener: sock, lock: lock}, nil
}

// Socket is one of the tool's sockets and the lock that gives it its name: they are released
// together, so nobody outlives the right to hold their socket.
type Socket struct {
	*net.UnixListener
	lock *os.File
}

func (s *Socket) Close() error { return errors.Join(s.UnixListener.Close(), s.lock.Close()) }

// Fds returns the descriptors to carry across a handover's exec: the socket, and its lock. The lock
// first — it is the right to carry that name, and releasing it for the time of the handover would
// let a client knock on nothing and launch a second session for the same configuration, which would
// take the place and make the new one exit on ErrTaken.
func (s *Socket) Fds() (listen, lock int, err error) {
	if listen, err = fdpass.Number(s.UnixListener); err != nil {
		return 0, 0, err
	}
	lock, err = fdpass.Number(s.lock)
	return listen, lock, err
}

// Adopt takes back a socket and its lock inherited from an exec: it never stopped accepting, and
// the lock was never released. The file is deleted on close, like the one Bind opens.
func Adopt(addr string, listen, lock int) (*Socket, error) {
	// net.FileListener duplicates the descriptor: the one that crossed over is of no further use.
	// It is released through its *os.File, sole owner of the number — closing the number on the
	// side would let the File's finalizer close it again at the first GC, when another file has
	// taken it.
	passed := os.NewFile(uintptr(listen), addr)
	held, err := net.FileListener(passed)
	passed.Close()
	if err != nil {
		return nil, err
	}
	sock, taken := held.(*net.UnixListener)
	if !taken {
		held.Close()
		return nil, fmt.Errorf("fd %d is not the unix socket of %s", listen, addr)
	}
	sock.SetUnlinkOnClose(true)
	return &Socket{UnixListener: sock, lock: os.NewFile(uintptr(lock), addr+".lock")}, nil
}

// hub is the table of sessions, in registration order, and the ports they keep held. Nothing in it
// is derived: a request's host is looked up every time, so a session that leaves releases its hosts
// without anybody having to take them back.
//
// A configuration appears in it only once, and a host has only one server per port: it is checked
// at registration, under this lock, and nobody downstream has any tie to break.
type hub struct {
	mu      sync.Mutex
	clients []*client
	ports   map[int]net.Listener
	// changed carries the number of sessions after each arrival and each departure: it is what
	// tells the hub when its life has no purpose anymore. live consumes it without ever taking the
	// table's lock, which allows writing it while holding the lock.
	changed chan int

	// born is this hub's birth: what to answer whoever finds their port taken and looks for whose
	// it is, its version coming from the binary itself.
	born time.Time
}

// client is a registered session: its control connection, the port it asks for, the configuration
// it comes from — its identity — and the hosts it serves. The hosts are replaced in one block, so a
// routing reads them without locking anything; the writes to the session are serialized for it
// alone, never under the table's lock, which a slow session would freeze.
type client struct {
	conn   *net.UnixConn
	port   int
	config string

	writing sync.Mutex
	hosts   atomic.Pointer[[]string]
}

// passPatience is the time a session is given to take what is written to it. Beyond it, it no
// longer reads: the line and its descriptor may be half sent, and there is nothing to recover
// behind a desynchronized socket — it is a departed client.
const passPatience = 2 * time.Second

// welcome welcomes the sessions. The control connection lives as long as the session lives: its
// close counts as unregistration, including when the session dies without warning.
func (h *hub) welcome(sock net.Listener) {
	for {
		conn, err := sock.Accept()
		if err != nil {
			return
		}
		go h.attend(conn.(*net.UnixConn))
	}
}

func (h *hub) attend(conn *net.UnixConn) {
	defer conn.Close()
	in := fdpass.NewReader(conn)
	greeting, err := in.Line()
	if err != nil {
		return
	}
	// The panel does not register, it asks: the hub is the only one to know which sessions are
	// alive, and that is all it has to say about them.
	if strings.TrimSpace(greeting) == "sessions" {
		h.roll(conn)
		return
	}
	// Who is there: the question another binary whose port is taken asks. A hub of a version that
	// does not know it closes without answering, and the caller does without.
	if strings.TrimSpace(greeting) == "who" {
		fmt.Fprintf(conn, "who %s %s\n", build.Version, h.born.Format(time.RFC3339))
		return
	}
	fields := strings.SplitN(greeting, " ", 5)
	if len(fields) != 5 || fields[0] != "hello" {
		fmt.Fprintf(conn, "err unreadable greeting: %q\n", greeting)
		return
	}
	if fields[1] != strconv.Itoa(Version) {
		fmt.Fprintf(conn, "err the hub speaks protocol version %d, not %s: "+
			"close the sessions using it, it leaves with the last one\n", Version, fields[1])
		return
	}
	port, err := strconv.Atoi(fields[2])
	if err != nil {
		fmt.Fprintf(conn, "err unreadable port: %q\n", fields[2])
		return
	}

	// The hosts and the port are taken before the acknowledgment: the session that receives "ok" is
	// already served.
	c, err := h.join(conn, port, fields[4], split(fields[3]))
	if err != nil {
		fmt.Fprintf(conn, "err %v\n", err)
		return
	}
	if _, err := fmt.Fprintln(conn, "ok"); err != nil {
		h.leave(c)
		return
	}
	defer h.leave(c)
	for {
		line, err := in.Line()
		if err != nil {
			return
		}
		if verb, rest, _ := strings.Cut(line, " "); verb == "hosts" {
			h.reassign(c, split(rest))
		}
	}
}

// roll tells the registered sessions, one per line, the configuration last: it alone can carry a
// space.
func (h *hub) roll(conn *net.UnixConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		fmt.Fprintf(conn, "session %d %s\n", c.port, c.config)
	}
}

// join registers the session, or says why it cannot be. Everything plays out under the same lock:
// two sessions arriving together with the same host would otherwise both believe they own it, and
// that is exactly what is refused.
func (h *hub) join(conn *net.UnixConn, port int, config string, hosts []string) (*client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		if c.config == config {
			return nil, fmt.Errorf("a session already serves %s", config)
		}
	}
	for _, host := range hosts {
		if owner := h.serving(port, host); owner != nil {
			// The port is already named by whoever will read this refusal: the session was asking
			// for it.
			return nil, fmt.Errorf("%s already belongs to %s", show(host), owner.config)
		}
	}
	if err := h.open(port); err != nil {
		return nil, err
	}
	c := &client{conn: conn, port: port, config: config}
	c.hosts.Store(&hosts)
	h.clients = append(h.clients, c)
	h.changed <- len(h.clients)
	return c, nil
}

func (h *hub) leave(gone *client) {
	h.mu.Lock()
	h.clients = slices.DeleteFunc(h.clients, func(c *client) bool { return c == gone })
	// The port goes away with the last session that claimed it: it belongs to nobody in between.
	if tcp := h.ports[gone.port]; tcp != nil && !h.wanted(gone.port) {
		tcp.Close()
		delete(h.ports, gone.port)
	}
	count := len(h.clients)
	h.mu.Unlock()
	h.changed <- count
}

// reassign replaces a session's hosts after a reload. A host already served by another one stays
// with that one, and the session learns it: it is running at this stage, and refusing it everything
// else because one name is contested would be worse than telling it.
func (h *hub) reassign(c *client, hosts []string) {
	kept := make([]string, 0, len(hosts))
	var refused []string
	h.mu.Lock()
	for _, host := range hosts {
		if owner := h.serving(c.port, host); owner != nil && owner != c {
			refused = append(refused, fmt.Sprintf("!! %s belongs to %s: this session will not serve it",
				show(host), owner.config))
			continue
		}
		kept = append(kept, host)
	}
	c.hosts.Store(&kept)
	h.mu.Unlock()
	for _, why := range refused {
		c.tell(why)
	}
}

// open takes the port for the first session that claims it. It stays open as long as a session
// wants it, so a second session on the same port never sees it closed. Called while holding the
// table's lock.
func (h *hub) open(port int) error {
	if _, held := h.ports[port]; held {
		return nil
	}
	tcp, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	h.ports[port] = tcp
	go h.route(port, tcp)
	return nil
}

// wanted says whether a session still claims this port. Called while holding the table's lock.
func (h *hub) wanted(port int) bool {
	for _, c := range h.clients {
		if c.port == port {
			return true
		}
	}
	return false
}

// serving returns the session that serves this host on this port. Called while holding the table's
// lock.
func (h *hub) serving(port int, host string) *client {
	for _, c := range h.clients {
		if c.port == port && c.serves(host) {
			return c
		}
	}
	return nil
}

// live waits until the hub has no reason to be anymore: the last session gone, or the first one
// never arrived.
func (h *hub) live() {
	patience := time.NewTimer(grace)
	defer patience.Stop()
	served := false
	for {
		select {
		case <-patience.C:
			if !served {
				return
			}
		case count := <-h.changed:
			if count > 0 {
				served = true
				patience.Stop()
				continue
			}
			if served {
				return
			}
		}
	}
}

func (c *client) serves(host string) bool {
	return slices.Contains(c.declared(), host)
}

func (c *client) declared() []string { return *c.hosts.Load() }

// pass hands a connection to the session.
func (c *client) pass(conn *net.TCPConn) error {
	return c.write(func(to *net.UnixConn) error { return fdpass.SendConn(to, "conn", conn) })
}

// tell makes a line be said in the session's log: it is the only path through which the hub talks
// to it once registered.
func (c *client) tell(news string) {
	c.write(func(to *net.UnixConn) error {
		_, err := fmt.Fprintf(to, "say %s\n", news)
		return err
	})
}

// write writes to the session, one write at a time: a descriptor travels with its line, and two
// interleaved sends would mix the two. A session that does not read within the delay leaves a
// socket whose stream position is no longer known: it is closed, which unregisters it through the
// ordinary path, and the connection goes back out as a 503 as for a host nobody serves.
func (c *client) write(send func(*net.UnixConn) error) error {
	c.writing.Lock()
	defer c.writing.Unlock()
	err := errors.Join(c.conn.SetWriteDeadline(time.Now().Add(passPatience)), send(c.conn))
	if err != nil {
		c.conn.Close()
	}
	return err
}

// route passes each connection of the port to the session that owns its host. The first to have
// declared it owns it, and the hub told the second one so by refusing it.
func (h *hub) route(port int, tcp net.Listener) {
	for {
		conn, err := tcp.Accept()
		if err != nil {
			return
		}
		go h.hand(port, conn.(*net.TCPConn))
	}
}

func (h *hub) hand(port int, conn *net.TCPConn) {
	defer conn.Close()
	host, err := peekHost(conn)
	if err != nil {
		reply(conn, "400 Bad Request", "unreadable request: "+err.Error())
		return
	}
	if owner := h.owner(port, host); owner != nil && owner.pass(conn) == nil {
		return
	}
	reply(conn, "503 Service Unavailable", h.roster(port, host))
}

// owner returns the session that serves this host on this port, or the one that takes everything
// nobody claims.
func (h *hub) owner(port int, host string) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.serving(port, host); c != nil {
		return c
	}
	return h.serving(port, anyHost)
}

// roster tells what is served on this port, so that the 503 answers the only question one asks in
// front of it: which name should have been typed?
func (h *hub) roster(port int, host string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var known []string
	for _, c := range h.clients {
		if c.port == port {
			known = append(known, c.declared()...)
		}
	}
	// The catch-all is not a name one could have typed, and this is only reached if nobody took it:
	// naming it would only help to get it wrong.
	known = slices.DeleteFunc(known, func(host string) bool { return host == anyHost })
	if len(known) == 0 {
		return fmt.Sprintf("no session serves port %d", port)
	}
	return fmt.Sprintf("no session serves %s; here: %s", host, strings.Join(known, ", "))
}

func reply(conn *net.TCPConn, status, message string) {
	// What arrived was only peeked at, never read: a socket closed on unread bytes goes out as an
	// RST, and the client loses the response just written. So it is swallowed before answering.
	drain(conn)
	body := message + "\n"
	fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, len(body), body)
}

// drain swallows in one go what peekHost had only peeked at, and nothing more: following the stream
// would mean being held as long as the client writes, and the public port is open to everyone. What
// remains after this buffer will go out as an RST, the response is already written when that
// happens. The read deadline is cleared first — expired, it would make the read return the error
// before even looking, and it is precisely when it expires that there are unread bytes to swallow.
func drain(conn *net.TCPConn) {
	conn.SetReadDeadline(time.Time{})
	raw, err := conn.SyscallConn()
	if err != nil {
		return
	}
	buf := make([]byte, 4096)
	raw.Read(func(fd uintptr) bool {
		unix.Read(int(fd), buf)
		return true
	})
}

// peekPatience bounds the whole wait for the headers, for the first byte as well as for the rest:
// the public port is open to everyone, and nobody holds a goroutine and a descriptor there longer
// than that. The deadline is held by the kernel, not by us.
const peekPatience = 2 * time.Second

// peekStep is the first wait between two peeks, and it then doubles without exceeding peekStepMax:
// the headers almost always arrive in a single segment, a dragging rest does not cost loop rounds,
// and the deadline is never overrun by more than one step.
const (
	peekStep    = 5 * time.Millisecond
	peekStepMax = 100 * time.Millisecond
)

// peekHost reads the request's Host without consuming it: the session will receive the connection
// intact, headers included, and will not have to know someone looked before it. The read deadline
// does not travel with the descriptor — it lives in our runtime, not in the socket.
func peekHost(conn *net.TCPConn) (string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(peekPatience)); err != nil {
		return "", err
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return "", err
	}
	buf := make([]byte, 4096)
	for wait := peekStep; ; wait = min(2*wait, peekStepMax) {
		var (
			n     int
			inner error
		)
		if err := raw.Read(func(fd uintptr) bool {
			n, _, inner = unix.Recvfrom(int(fd), buf, unix.MSG_PEEK)
			return inner != unix.EAGAIN && inner != unix.EWOULDBLOCK
		}); err != nil {
			return "", err
		}
		if inner != nil {
			return "", inner
		}
		if n == 0 {
			return "", errors.New("connection closed before the end of the headers")
		}
		if !startsRequest(buf[:n]) {
			return "", errors.New("not an HTTP request")
		}
		if host, found := hostIn(buf[:n]); found {
			return host, nil
		}
		if n >= len(buf) {
			return "", fmt.Errorf("headers longer than %d bytes", len(buf))
		}
		// There is nothing else to wait for but the rest of the headers; the read deadline exits
		// the loop without being watched.
		time.Sleep(wait)
	}
}

// longestMethod bounds a request's method: uppercase letters and dashes, and no longer than the
// most verbose ones WebDAV has invented (BASELINE-CONTROL has sixteen).
const longestMethod = 24

// startsRequest says whether these bytes can start an HTTP request: a method, then a space. A
// method is written in uppercase, dashes allowed, and does not exceed longestMethod. A TLS
// ClientHello, or any other protocol that ended up here by mistake, therefore shows in the first
// segment, instead of making us wait for headers that will never come. Too few bytes to decide
// counts as true: the rest will tell.
func startsRequest(head []byte) bool {
	for i, b := range head {
		switch {
		case b == ' ':
			return i > 0
		case i >= longestMethod, b != '-' && (b < 'A' || b > 'Z'):
			return false
		}
	}
	return true
}

// hostIn looks for the Host header in what arrived. The second return value says whether the answer
// is final: the header found, or the end of the headers reached without it — an HTTP/1.0 request is
// allowed that, and then the facade without a host answers. Lines are split on the bare line feed,
// the carriage return that usually precedes it being optional for whoever writes by hand.
func hostIn(head []byte) (string, bool) {
	text := string(head)
	for {
		line, rest, cut := strings.Cut(text, "\n")
		if !cut {
			return "", false
		}
		if line = strings.TrimSuffix(line, "\r"); line == "" {
			return "", true // end of headers, no Host
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "host") {
			host := strings.ToLower(strings.TrimSpace(value))
			if bare, _, err := net.SplitHostPort(host); err == nil {
				host = bare
			}
			return host, true
		}
		text = rest
	}
}

// split reads a list of hosts from the protocol. The list is whole every time: there is no
// difference to apply, hence no possible disagreement between the two sides.
func split(list string) []string {
	var hosts []string
	for _, host := range strings.Split(list, ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// show names a host for whoever reads a message: the facade without a host has no name in the
// configuration, and its protocol name would mean nothing.
func show(host string) string {
	if host == anyHost {
		return "the facade with no host"
	}
	return host
}
