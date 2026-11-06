package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
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
// runs without it, as before. A session has no terminal to hold: the unit returns as soon as it is
// started, and the session talks through its socket.
func box() {
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
	args := []string{"systemd-run", "--user", "--quiet", "--collect",
		"--same-dir",
		"--description=" + filepath.Base(exe) + " — " + cwd,
		// Stopping belongs to the session: SIGTERM to the main process alone, which stops its apps in
		// order and releases the port. systemd's default sends it to the whole cgroup at once, and the
		// dev servers would die under the feet of whoever supervises them. The group's SIGKILL remains,
		// after the timeout — five seconds per app, plus margin.
		"--property=KillMode=mixed",
		"--property=TimeoutStopSec=30",
		// The unit would fall back to systemd's low limit (1024, measured): keep the shell's, a dev
		// server watching a whole project needs it.
		"--property=LimitNOFILE=" + nofile(),
		"--setenv=" + boxEnv + "=" + inBox,
	}
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

// takeLoginEnv puts the service in the environment a terminal of the user would have, and everything
// it starts afterwards inherits it — the session, its apps, the update. A shell that fails leaves the
// manager's environment, and the service log says so once.
func takeLoginEnv() {
	env, err := loginEnv(loginShellDelay)
	if err != nil {
		fmt.Fprintln(os.Stderr, "-- keeping the environment of the service manager: "+err.Error())
		return
	}
	os.Clearenv()
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		_ = os.Setenv(key, value)
	}
}

// loginShellDelay bounds the wait for the login shell: an rc waiting for input does not hold the
// icon back.
const loginShellDelay = 10 * time.Second

// loginEnv returns the environment given by the user's interactive login shell. The user's systemd
// manager only has the system PATH, and ~/.bashrc — where nvm, pnpm and the others set theirs —
// returns early unless read by an interactive shell: a service born from the unit would not find
// node. The shell returns its environment on descriptor 3, because stdout belongs to its rc files
// (motd, fastfetch) and stderr to bash, which complains about the missing terminal.
func loginEnv(delay time.Duration) ([]string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer read.Close()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer null.Close()

	ctx, done := context.WithTimeout(context.Background(), delay)
	defer done()
	login := exec.CommandContext(ctx, shell, "-ilc", "exec env -0 >&3")
	login.Stdin, login.Stdout, login.Stderr = null, null, null
	login.ExtraFiles = []*os.File{write}
	// A lingering shell must not outlive the tool, just like the configuration's node.
	login.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := login.Start(); err != nil {
		write.Close()
		return nil, err
	}
	// Our end of the pipe closes before reading: otherwise the read would wait for an end nobody writes
	// anymore. An rc that leaves a daemon behind, inheriting descriptor 3, would hold it the same way —
	// hence the same deadline on the read as on the shell.
	write.Close()
	deadline, _ := ctx.Deadline()
	_ = read.SetReadDeadline(deadline)
	said, err := io.ReadAll(read)
	if waited := login.Wait(); waited != nil {
		return nil, fmt.Errorf("%s: %w", shell, cmp.Or(ctx.Err(), waited))
	}
	if err != nil {
		return nil, err
	}

	var env []string
	for _, kv := range strings.Split(string(said), "\x00") {
		if key, _, ok := strings.Cut(kv, "="); ok && key != "" {
			env = append(env, kv)
		}
	}
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		return nil, errors.New(shell + " gave an environment without PATH")
	}
	return env, nil
}
