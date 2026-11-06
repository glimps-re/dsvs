// Package fdpass carries a file descriptor from one process to another over a Unix socket: the
// kernel reinstalls it in the receiver, which gets the same resource and not a copy of its bytes.
// It is what lets a port opened in one namespace be served from another, and a connection accepted
// by the hub be served by the session that owns the host.
package fdpass

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"

	"golang.org/x/sys/unix"
)

// Send writes a line and attaches the descriptors that go with it. They travel together or not at
// all: a terminal is its input and its output, and one half means nothing.
func Send(conn *net.UnixConn, line string, fds ...int) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sendErr error
	err = raw.Write(func(socket uintptr) bool {
		sendErr = unix.Sendmsg(int(socket), []byte(line+"\n"), unix.UnixRights(fds...), nil, 0)
		// The socket is non-blocking: as long as it accepts nothing, the runtime is left to wait.
		return sendErr != unix.EAGAIN && sendErr != unix.EWOULDBLOCK
	})
	return errors.Join(err, sendErr)
}

// Recv returns what a recvmsg read: the received text and, if any come with it, the descriptors
// that go with it. The kernel does not keep write boundaries, so two messages sent close together
// arrive stuck together as several lines — it is up to the caller to split them. An ended stream
// returns io.EOF: "nothing received, and no reason" is not a state, so the peer's death is always
// told, and never by an empty line a caller would take for a message.
func Recv(conn *net.UnixConn) (string, []int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return "", nil, err
	}
	var (
		line  string
		fds   []int
		inner error
	)
	err = raw.Read(func(socket uintptr) bool {
		buf := make([]byte, 4096)
		oob := make([]byte, unix.CmsgSpace(4*maxFds))
		// MSG_CMSG_CLOEXEC, otherwise the received descriptors leak into every process the receiver
		// launches afterwards: measured, an app held the attached client's terminal on top of its
		// own, and a connection passed by the hub would stay open there the same way.
		n, oobn, _, _, e := unix.Recvmsg(int(socket), buf, oob, unix.MSG_CMSG_CLOEXEC)
		if e == unix.EAGAIN || e == unix.EWOULDBLOCK {
			return false // nothing has arrived yet: the runtime will call back when the socket is readable
		}
		if e != nil {
			inner = e
			return true
		}
		if n == 0 && oobn == 0 {
			inner = io.EOF
			return true
		}
		line = strings.TrimSpace(string(buf[:n]))
		fds, inner = rights(oob[:oobn])
		return true
	})
	return line, fds, errors.Join(err, inner)
}

// Reader reads a stream of lines and the descriptors that go with them. A stream socket does not
// keep write boundaries: two lines written in a row arrive stuck together, a line arrives cut in
// two, and the kernel delivers with them the descriptors of the sends it stuck together. Framing is
// therefore a protocol rule, and it lives here alone: each Line returns a whole line, and Fd
// returns the descriptors in their order of arrival, the very order of their lines.
//
// The reader belongs to the goroutine that reads: only it touches the received descriptors, and it
// is the one that gives back to the kernel those nobody came to take, as soon as the stream returns
// nothing more. From anywhere else, there is only Close, which breaks the socket and nothing else.
type Reader struct {
	conn *net.UnixConn
	rest []byte
	fds  []int
}

func NewReader(conn *net.UnixConn) *Reader { return &Reader{conn: conn} }

// Conn returns the socket, to write on it: framing only concerns reading.
func (r *Reader) Conn() *net.UnixConn { return r.conn }

// Close breaks the socket, and leaves the received descriptors to whoever reads them: the ongoing
// read then returns an error, and that is where they are closed. A reader can therefore be closed
// from any goroutine without ever racing for a descriptor number the other one is taking.
func (r *Reader) Close() error { return r.conn.Close() }

// Line returns the next whole line, reading the socket as many times as needed.
func (r *Reader) Line() (string, error) {
	for {
		if line, rest, found := bytes.Cut(r.rest, []byte("\n")); found {
			r.rest = rest
			return strings.TrimSpace(string(line)), nil
		}
		if err := r.fill(); err != nil {
			// Nothing will come to take them anymore: what arrived and was not asked for goes back
			// to the kernel, and the connection that was waiting behind closes with it.
			for _, fd := range r.fds {
				unix.Close(fd)
			}
			r.fds = nil
			return "", err
		}
	}
}

// Fd returns the next arrived descriptor. The second return value says whether there was one: a
// line whose descriptor is missing is a peer that does not speak the protocol, never a read too
// early — the kernel delivers a send's descriptors with its bytes.
func (r *Reader) Fd() (int, bool) {
	if len(r.fds) == 0 {
		return 0, false
	}
	fd := r.fds[0]
	r.fds = r.fds[1:]
	return fd, true
}

func (r *Reader) fill() error {
	raw, err := r.conn.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	err = raw.Read(func(socket uintptr) bool {
		buf := make([]byte, 4096)
		oob := make([]byte, unix.CmsgSpace(4*maxFds))
		n, oobn, _, _, e := unix.Recvmsg(int(socket), buf, oob, unix.MSG_CMSG_CLOEXEC)
		switch {
		case e == unix.EAGAIN || e == unix.EWOULDBLOCK:
			return false // nothing has arrived yet: the runtime will call back when the socket is readable
		case e != nil:
			inner = e
			return true
		case n == 0 && oobn == 0:
			inner = io.EOF
			return true
		}
		r.rest = append(r.rest, buf[:n]...)
		fds, e := rights(oob[:oobn])
		r.fds = append(r.fds, fds...)
		inner = e
		return true
	})
	return errors.Join(err, inner)
}

// rights reads the descriptors of an ancillary data block. No message is the ordinary case: the
// kernel delivers bytes without attaching anything, and a line does not always have a descriptor.
func rights(oob []byte) ([]int, error) {
	if len(oob) == 0 {
		return nil, nil
	}
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	for _, message := range messages {
		passed, err := unix.ParseUnixRights(&message)
		if err != nil {
			return nil, err
		}
		fds = append(fds, passed...)
	}
	return fds, nil
}

// maxFds is what a single message can carry: a send attaches only one or two descriptors, but the
// kernel sticks sends together, and the missing room would be lost descriptors.
const maxFds = 253

// SendConn hands over an already accepted connection: the receiver takes it as is, unread headers
// included. The sender keeps its own to close, the kernel does not count the same socket twice.
func SendConn(to *net.UnixConn, line string, conn *net.TCPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sendErr error
	if err := raw.Control(func(fd uintptr) { sendErr = Send(to, line, int(fd)) }); err != nil {
		return err
	}
	return sendErr
}
