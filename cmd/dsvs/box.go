package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// boxEnv says whether the box question is settled, and how: inBox when inside it, otherwise the
// reason to go without. Children inherit it, so the re-executed supervisor does not box itself again.
const (
	boxEnv = "DSVS_BOXED"
	inBox  = "1"
)

// box re-executes the binary as a transient unit of the user's systemd session, and does not
// return. systemd puts everything descending from the unit in a cgroup — a list kept by the kernel,
// which even a process that detaches itself cannot leave — and kills whatever remains in it when the
// main process dies, kill -9 included (measured, systemd 257). That is the guarantee that a brutal
// stop leaves no orphan apps; Pdeathsig only covers the processes we start ourselves, not those nx
// detaches. When the box is out of reach, box returns and leaves the reason in boxEnv: the session
// runs without it, as before.
func box(detached bool) {
	if os.Getenv(boxEnv) != "" {
		return
	}
	stay := func(reason string) { _ = os.Setenv(boxEnv, reason) }
	runner, err := exec.LookPath("systemd-run")
	if err != nil {
		stay("systemd-run not found")
		return
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "systemd", "private")); err != nil {
		stay("no systemd user session")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		stay(err.Error())
		return
	}

	cwd, _ := os.Getwd()
	args := []string{"systemd-run", "--user", "--quiet", "--collect"}
	if !detached {
		// Both at once: systemd takes the pty when facing a terminal, the pipe otherwise. The window
		// size and its changes go through the pty. A session without an interface has no terminal to
		// hold: it returns as soon as it is started, and talks through its socket.
		args = append(args, "--wait", "--pty", "--pipe")
	}
	args = append(args,
		"--same-dir",
		"--description="+filepath.Base(exe)+" — "+cwd,
		// Stopping belongs to the session: SIGTERM to the main process alone, which stops its apps in
		// order and releases the port. systemd's default sends it to the whole cgroup at once, and the
		// dev servers would die under the feet of whoever supervises them. The group's SIGKILL remains,
		// after the timeout — five seconds per app, plus margin.
		"--property=KillMode=mixed",
		"--property=TimeoutStopSec=30",
		// The unit would fall back to systemd's low limit (1024, measured): keep the shell's, a dev
		// server watching a whole project needs it.
		"--property=LimitNOFILE="+nofile(),
		"--setenv="+boxEnv+"="+inBox,
	)
	// The unit is born with the manager's environment, not the shell's: everything must be carried
	// over explicitly. Values go through argv, never through a shell, so nothing needs escaping.
	for _, kv := range os.Environ() {
		args = append(args, "--setenv="+kv)
	}
	args = append(args, "--", exe)
	args = append(args, os.Args[1:]...)

	// The terminal title belongs to the user: without this, systemd-run rewrites it.
	env := append(os.Environ(), "SYSTEMD_ADJUST_TERMINAL_TITLE=0")
	if err := syscall.Exec(runner, args, env); err != nil {
		stay(err.Error())
	}
}

// nofile returns the current file descriptor limit, in the soft:hard format of LimitNOFILE=.
func nofile() string {
	var l syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &l); err != nil {
		return "524288"
	}
	return fmt.Sprintf("%d:%d", l.Cur, l.Max)
}
