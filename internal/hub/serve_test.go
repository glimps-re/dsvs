package hub

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// crossEnv makes this test binary a handover's session: "hand" takes the socket and execs itself,
// "take" takes back what crossed over.
const crossEnv = "DSVS_TEST_CROSSING"

// A session's socket and its lock cross a handover's exec. It is the trap one does not see: with
// the lock released, a client that knocks during the handover finds nobody and launches a second
// session for the same configuration, which takes the name from under the new one.
func TestTheSocketAndItsLockCrossAnExec(t *testing.T) {
	addr := filepath.Join(t.TempDir(), "session.sock")
	crossing := exec.Command(os.Args[0], "-test.run=TestCrossingProcess")
	crossing.Env = append(os.Environ(), crossEnv+"=hand", crossEnv+"_ADDR="+addr)
	crossing.Stderr = os.Stderr
	if err := crossing.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		crossing.Process.Kill()
		crossing.Wait()
	})

	// The new image answers behind the socket the old one had opened, without having reopened it.
	var said string
	waitFor(t, func() bool {
		conn, err := net.Dial("unix", addr)
		if err != nil {
			return false
		}
		defer conn.Close()
		said, _ = bufio.NewReader(conn).ReadString('\n')
		return said != ""
	})
	if said != "adopted\n" {
		t.Errorf("the socket answers %q, want the new image to have taken it back", said)
	}

	// And the lock is still taken: nobody else can carry that name.
	if _, err := Bind(addr); !errors.Is(err, ErrTaken) {
		t.Errorf("taking the socket returns %v, want %v: the lock was released in the exec", err, ErrTaken)
	}
}

func TestCrossingProcess(t *testing.T) {
	addr := os.Getenv(crossEnv + "_ADDR")
	switch os.Getenv(crossEnv) {
	case "hand":
		sock, err := Bind(addr)
		if err != nil {
			t.Fatal(err)
		}
		listen, lock, err := sock.Fds()
		if err != nil {
			t.Fatal(err)
		}
		for _, fd := range []int{listen, lock} {
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags&^unix.FD_CLOEXEC); err != nil {
				t.Fatal(err)
			}
		}
		// The variables replace each other instead of stacking: two entries of the same name would
		// let the first decide, and the new image would take back the old one's role.
		os.Setenv(crossEnv, "take")
		os.Setenv(crossEnv+"_FDS", strconv.Itoa(listen)+","+strconv.Itoa(lock))
		t.Fatal(unix.Exec(os.Args[0], os.Args, os.Environ()))

	case "take":
		var listen, lock int
		if _, err := fmt.Sscanf(os.Getenv(crossEnv+"_FDS"), "%d,%d", &listen, &lock); err != nil {
			t.Fatal(err)
		}
		sock, err := Adopt(addr, listen, lock)
		if err != nil {
			t.Fatal(err)
		}
		defer sock.Close()
		// The lock was not released: this process is the only one holding it, and taking it again
		// fails.
		if _, err := Bind(addr); !errors.Is(err, ErrTaken) {
			t.Fatalf("taking the socket returns %v, want %v", err, ErrTaken)
		}
		// The new image stays there: it is by keeping it alive that the other process can see the
		// lock was not released.
		for {
			conn, err := sock.Accept()
			if err != nil {
				return
			}
			fmt.Fprintln(conn, "adopted")
			conn.Close()
		}

	default:
		t.Skip("the test binary only hands over on request")
	}
}

// The number that crossed the handover has only one owner: the *os.File Adopt uses to take the
// socket back, closed on the spot. Leaving it to the garbage collector would make its finalizer
// close that number much later, when another file has taken it — on the owner's workstation, the
// pidfd through which an app is waited for, hence a "waitid: bad file descriptor" and an app
// declared exited while it still runs.
func TestAdoptLeavesTheHeirNumberToItsNextOwner(t *testing.T) {
	addr := filepath.Join(t.TempDir(), "session.sock")
	sock, err := Bind(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	listen, lock, err := sock.Fds()
	if err != nil {
		t.Fatal(err)
	}
	// A handover's exec leaves nothing of the old image: what crosses over is a bare number, whose
	// sole owner is the adoption. Here the old image stays, and the duplication plays that role.
	heir, err := unix.Dup(listen)
	if err != nil {
		t.Fatal(err)
	}
	heirLock, err := unix.Dup(lock)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := Adopt(addr, heir, heirLock)
	if err != nil {
		t.Fatal(err)
	}
	defer adopted.Close()

	// The released number comes back to the next file, exactly as to the pidfd the takeover opens.
	if err := unix.Dup3(int(adopted.lock.Fd()), heir, 0); err != nil {
		t.Fatalf("fd %d is not free after the adoption: %v", heir, err)
	}
	defer unix.Close(heir)

	// Two GCs and the time for the finalizers to run: the new owner must survive them. The double
	// close, for its part, falls on the first round.
	for range 20 {
		runtime.GC()
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
		if _, err := unix.FcntlInt(uintptr(heir), unix.F_GETFD, 0); err != nil {
			t.Fatalf("fd %d closed under its new owner: %v", heir, err)
		}
	}
}
