package fdpass

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A stream socket does not keep write boundaries: a line can arrive in two pieces, and it is up to
// the reader to stitch it back.
func TestReaderJoinsALineSplitInTwoSegments(t *testing.T) {
	sender, receiver := pair(t)
	in := NewReader(receiver)

	if _, err := sender.Write([]byte("con")); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		sender.Write([]byte("n\n"))
	}()

	if line, err := in.Line(); line != "conn" || err != nil {
		t.Errorf("Line = %q, %v; want \"conn\"", line, err)
	}
}

// Two sends stuck together keep their order, and each line finds the descriptor that left with it:
// what the reader writes into a line's descriptor comes out through that line's pipe.
func TestReaderKeepsEachDescriptorWithItsLine(t *testing.T) {
	sender, receiver := pair(t)
	sent := []struct {
		line string
		tube tube
	}{
		{"one", newTube(t)},
		{"two", newTube(t)},
	}
	for _, message := range sent {
		if err := Send(sender, message.line, int(message.tube.write.Fd())); err != nil {
			t.Fatal(err)
		}
	}

	in := NewReader(receiver)
	for _, message := range sent {
		line, err := in.Line()
		if err != nil {
			t.Fatal(err)
		}
		fd, passed := in.Fd()
		if !passed {
			t.Fatalf("%q arrived without a descriptor", line)
		}
		unix.Write(fd, []byte(line))
		unix.Close(fd)
		if got := message.tube.text(t); got != message.line {
			t.Errorf("the pipe of %q received %q: the descriptors changed lines", message.line, got)
		}
	}
}

// An ordinary line has no descriptor, and the reader says so.
func TestReaderReadsALineWithoutDescriptor(t *testing.T) {
	sender, receiver := pair(t)
	if _, err := sender.Write([]byte("hosts one.test,two.test\n")); err != nil {
		t.Fatal(err)
	}

	in := NewReader(receiver)
	if line, err := in.Line(); line != "hosts one.test,two.test" || err != nil {
		t.Fatalf("Line = %q, %v", line, err)
	}
	if fd, passed := in.Fd(); passed {
		t.Errorf("Fd = %d, want none", fd)
	}
}

// A stream that returns nothing more also gives back the descriptors nobody came to take: otherwise
// the session keeps one open and whoever waits behind it is never served.
func TestReaderClosesTheDescriptorsNobodyTook(t *testing.T) {
	sender, receiver := pair(t)
	abandoned := newTube(t)
	if err := Send(sender, "conn", int(abandoned.write.Fd())); err != nil {
		t.Fatal(err)
	}

	in := NewReader(receiver)
	if line, err := in.Line(); line != "conn" || err != nil {
		t.Fatalf("Line = %q, %v", line, err)
	}
	sender.Close()
	if line, err := in.Line(); err == nil {
		t.Fatalf("Line = %q, %v; want the end of the stream", line, err)
	}

	abandoned.write.Close()
	if !abandoned.drained(t) {
		t.Error("the pipe stayed open: the received descriptor leaked")
	}
}

// The reader belongs to the goroutine that reads. A session that closes while a connection arrives
// is an ordinary path: the close comes from elsewhere, and must not race for a descriptor the
// reading goroutine is taking.
func TestReaderCanBeClosedWhileDescriptorsArrive(t *testing.T) {
	sender, receiver := pair(t)
	carried := newTube(t)
	in := NewReader(receiver)

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for range 200 {
			if err := Send(sender, "conn", int(carried.write.Fd())); err != nil {
				return
			}
		}
	}()
	read := make(chan struct{})
	go func() {
		defer close(read)
		for {
			if _, err := in.Line(); err != nil {
				return
			}
			if fd, passed := in.Fd(); passed {
				unix.Close(fd)
			}
		}
	}()

	time.Sleep(time.Millisecond)
	in.Close()
	<-read
	<-sent
}

// Recv reads the message of a socket that carries only one: that of the private network's
// gatekeeper, with its descriptor, or an answer that attaches none.
func TestRecvReadsOneMessageWithOrWithoutDescriptor(t *testing.T) {
	sender, receiver := pair(t)
	carried := newTube(t)
	if err := Send(sender, "tun 10.0.0.0/24", int(carried.write.Fd())); err != nil {
		t.Fatal(err)
	}

	line, fds, err := Recv(receiver)
	if line != "tun 10.0.0.0/24" || len(fds) != 1 || err != nil {
		t.Fatalf("Recv = %q, %v, %v", line, fds, err)
	}
	unix.Write(fds[0], []byte("the tun"))
	unix.Close(fds[0])
	if got := carried.text(t); got != "the tun" {
		t.Errorf("the pipe received %q", got)
	}

	if _, err := sender.Write([]byte("ok\n")); err != nil {
		t.Fatal(err)
	}
	if line, fds, err := Recv(receiver); line != "ok" || len(fds) != 0 || err != nil {
		t.Errorf("Recv = %q, %v, %v; want \"ok\" without a descriptor", line, fds, err)
	}
}

// An ended stream is told by an error, never by an empty line a caller would take for a message: it
// is what tells the peer's death apart from its silence.
func TestRecvSaysTheStreamEnded(t *testing.T) {
	sender, receiver := pair(t)
	sender.Close()

	if line, _, err := Recv(receiver); !errors.Is(err, io.EOF) {
		t.Errorf("Recv = %q, %v; want io.EOF", line, err)
	}
}

// pair returns both ends of a stream socket, like the one linking a session to the hub.
func pair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	return fromFd(t, fds[0]), fromFd(t, fds[1])
}

func fromFd(t *testing.T, fd int) *net.UnixConn {
	t.Helper()
	file := os.NewFile(uintptr(fd), "pair")
	defer file.Close()
	conn, err := net.FileConn(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn.(*net.UnixConn)
}

// tube is a descriptor that can be carried, and a way to see what comes out of it: what is written
// into the received descriptor is read here, and its close shows here too.
type tube struct {
	read  *os.File
	write *os.File
}

func newTube(t *testing.T) tube {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { read.Close(); write.Close() })
	return tube{read: read, write: write}
}

func (u tube) text(t *testing.T) string {
	t.Helper()
	u.read.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, err := u.read.Read(buf)
	if err != nil {
		t.Fatalf("nothing came out of the pipe: %v", err)
	}
	return string(buf[:n])
}

// drained says nobody holds the writing end anymore: reading there ends instead of waiting.
func (u tube) drained(t *testing.T) bool {
	t.Helper()
	u.read.SetReadDeadline(time.Now().Add(time.Second))
	_, err := u.read.Read(make([]byte, 1))
	return errors.Is(err, io.EOF)
}
