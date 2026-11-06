// Package fdpass carries a file descriptor from one process to another over a Unix socket: the
// kernel reinstalls it in the receiver, which gets the same resource and not a copy of its bytes.
// It is what lets a port opened in one namespace be served from another.
package fdpass

import (
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
