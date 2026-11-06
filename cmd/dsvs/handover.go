package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"dsvs/internal/attach"
	"dsvs/internal/build"
	"dsvs/internal/core"
	"dsvs/internal/fdpass"
	"dsvs/internal/update"
)

// handoverEnv carries the descriptor of a handover's manifest: its presence tells the binary that it
// takes over a running session instead of opening one.
const handoverEnv = "DSVS_HANDOVER"

// renewal returns the move that replaces the session with the binary on disk, or nothing when it is out
// of reach: outside the systemd box, the apps carry Pdeathsig and the exec would kill them (measured: 2
// survivors out of 20), and a session without this move refuses the verb and says so. One held by a
// terminal of dsvs run refuses it too: it belongs to that terminal, which would see it go away before
// its eyes.
func renewal(session *core.Session, attachers *attach.Server, network core.Network) func() error {
	if os.Getenv(boxEnv) != inBox {
		return nil
	}
	return func() error {
		if attachers.Held() {
			return fmt.Errorf("this session belongs to the terminal of %s run: it keeps its binary until it stops", build.Name())
		}
		return handOver(session, attachers, network)
	}
}

// handOver replaces the session with the binary on disk without letting go of its apps: it execs
// itself, keeps its PID, hence its cgroup and its children, and the new image adopts them on the other
// side. Nothing is torn down before the exec — a failure leaves the session exactly where it was.
func handOver(session *core.Session, attachers *attach.Server, network core.Network) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// The binary is tried before the apps are entrusted to it: one that does not start does not take
	// over, and the session keeps its own.
	if _, err := update.VersionOf(exe); err != nil {
		return fmt.Errorf("%s does not run: %w", filepath.Base(exe), err)
	}

	taken, err := session.Export()
	if err != nil {
		return err
	}
	if taken.Listen, taken.Lock, err = attachers.Fds(); err != nil {
		return err
	}
	// The address plan of the private network, when there is one: the addresses are in the kernel, only
	// the app → address link is missing on the other side.
	if planner, ok := network.(interface{ Plan() map[string]string }); ok {
		taken.IPs = planner.Plan()
	}
	manifest, err := memfd(taken)
	if err != nil {
		return err
	}
	defer manifest.Close()

	// What crosses the exec of a handover, and this is the only place that says so: the apps'
	// terminals, the attach socket, its lock, and the manifest.
	fds := []int{taken.Listen, taken.Lock, int(manifest.Fd())}
	for _, app := range taken.Apps {
		if app.Run != nil {
			fds = append(fds, app.Run.TTY)
		}
	}
	if err := fdpass.Crossing(true, fds...); err != nil {
		return err
	}

	// The gatekeeper of the private network moves to the new binary last of the two that can still refuse,
	// once nothing else can fail here: its refusal leaves the session exactly where it was, and nothing
	// has been torn down. Its exec closes the Watch connection, which the session's new image reopens.
	gate, _ := network.(interface{ Renew() error })
	if gate != nil {
		if err := gate.Renew(); err != nil {
			_ = fdpass.Crossing(false, fds...)
			return fmt.Errorf("the gatekeeper of the private network keeps its binary: %w", err)
		}
	}
	attachers.Reloading()
	err = syscall.Exec(exe, os.Args, append(os.Environ(), handoverEnv+"="+strconv.Itoa(int(manifest.Fd()))))
	// The exec failed, and the session goes on: what was to cross gets its FD_CLOEXEC back, otherwise
	// the next app started would inherit the session's socket and its neighbours' terminals.
	_ = fdpass.Crossing(false, fds...)
	if gate != nil {
		// The gatekeeper moved over, the session did not: that is consistent — each serves with its own
		// binary — but it cannot be guessed, and the session log is where one looks for it.
		return fmt.Errorf("the gatekeeper of the private network is on the new binary, this session is not: %w", err)
	}
	return err
}

// memfd writes the manifest in an anonymous in-memory file: nothing on disk, nothing to clean up if the
// exec fails, no size to watch, and it goes away with the last process holding it. The returned file
// must live until the exec — letting it be collected would close the descriptor.
func memfd(taken core.Handover) (*os.File, error) {
	text, err := json.Marshal(taken)
	if err != nil {
		return nil, err
	}
	fd, err := unix.MemfdCreate("dsvs-handover", 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), handoverEnv)
	if _, err := file.Write(text); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// taking returns the manifest inherited from a handover, and nothing when the session opens normally.
// The variable leaves the environment on the way: the descriptor is valid this time only, and the apps
// have no business inheriting it.
func taking() (*core.Handover, error) {
	value, passed := os.LookupEnv(handoverEnv)
	if !passed {
		return nil, nil
	}
	os.Unsetenv(handoverEnv)
	fd, err := strconv.Atoi(value)
	if err != nil {
		return nil, fmt.Errorf("%s=%q: %w", handoverEnv, value, err)
	}
	manifest := os.NewFile(uintptr(fd), handoverEnv)
	defer manifest.Close()
	text, err := io.ReadAll(manifest)
	if err != nil {
		return nil, fmt.Errorf("unreadable handover: %w", err)
	}
	var taken core.Handover
	if err := json.Unmarshal(text, &taken); err != nil {
		return nil, fmt.Errorf("unreadable handover: %w", err)
	}
	return &taken, nil
}
