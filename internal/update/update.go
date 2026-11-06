package update

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"dsvs/internal/attach"
	"dsvs/internal/build"
	"dsvs/internal/hub"
)

// patience bounds the wait for a repository or a binary: the clone answers at once, GitHub not
// always, and a machine without a network never does.
const patience = 20 * time.Second

// Newer is the version that would be taken, empty when the running one is already right.
func Newer() (string, error) {
	if build.Origin == "" {
		return published()
	}
	return buildable()
}

// Apply takes that version: fetch, build or download, replace, restart. step receives each step as
// it begins, and nothing else — that is what a button shows while it works, and what failed comes
// back as an error.
// The old binary is kept beside it, and takes its place back if the new one does not start.
func Apply(step func(string)) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return apply(exe, step)
}

// apply replaces that binary. Apply gives it the running one; the test gives it a disposable one,
// rather than letting it rewrite the one running the test.
func apply(exe string, step func(string)) error {
	lock, err := alone(exe)
	if err != nil {
		return err
	}
	defer lock.Close()

	kept := exe + ".old"
	step("keeping " + filepath.Base(kept))
	if err := copyTo(exe, kept); err != nil {
		return err
	}
	// A failing install may already have replaced it — build.sh ends by running what it just laid
	// down. What matters is therefore not how it failed but which binary is there: it is checked in
	// every case, and the old one takes its place back if it does not start.
	failed := install(step, exe)

	step("checking the new binary")
	version, err := VersionOf(exe)
	if err != nil {
		step("it does not run: the old one comes back")
		if back := os.Rename(kept, exe); back != nil {
			return fmt.Errorf("%s does not run and could not be put back: %v", filepath.Base(exe), back)
		}
		// How the install failed adds nothing here: what was observed is closer to the truth, and it is a
		// binary that does not start.
		return fmt.Errorf("the new binary does not run, the old one is back: %w", err)
	}
	// The binary on disk alone decides: the previous one means nothing was replaced, and the install
	// then says why. A new one in place is an update done, even if the install stumbled after laying
	// it down — on the unit, for instance.
	if version == build.Version {
		if failed != nil {
			return failed
		}
		return fmt.Errorf("%s is still %s: nothing new was installed", filepath.Base(exe), version)
	}
	// The sessions move to the new binary before the service: the panel lives in the service, and it
	// vanishes in its exec — whatever came after would have no one left to report to.
	handedOver(step, version)
	if err := restarted(step); err != nil {
		return fmt.Errorf("%s is now %s, but the service kept the old one: %w", filepath.Base(exe), version, err)
	}
	step("now " + version + also(failed))
	return nil
}

// also appends to the final step what the install said when it stumbled after laying down the binary
// — on the unit, for instance. The update is done, and that is still worth knowing.
func also(failed error) string {
	if failed == nil {
		return ""
	}
	return " (the install also said: " + firstLine(failed.Error()) + ")"
}

// alone holds the update lock, set on the resource two updates would fight over: the clone when the
// binary comes from one — two binaries born from the same clone would merge and build there at the
// same time —, the binary itself otherwise, when it comes from a release. A single lock, then, and
// never two to take in a set order. The kernel releases it when the process goes. Only EWOULDBLOCK
// says another one holds it: other failures — a lock impossible on this mount, for instance — are
// reported as they are rather than announcing a contention that does not exist.
func alone(exe string) (*os.File, error) {
	shared := exe + ".lock"
	if build.Origin != "" {
		shared = filepath.Join(build.Origin, ".update.lock")
	}
	lock, err := os.OpenFile(shared, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return nil, err
		}
		return nil, fmt.Errorf("another update is already running: %s is taken", shared)
	}
	return lock, nil
}

// restarted asks the service to replace itself with the new binary, and waits to see it back on it.
// The service vanishes in its exec: the connection drops without a word, and that is the
// acknowledgement. A word, on the contrary, is what kept it from leaving.
func restarted(step func(string)) error {
	conn, err := net.Dial("unix", hub.TrayAddr())
	if err != nil {
		return nil // no service running: nothing to restart, and nothing to say about it
	}
	defer conn.Close()
	step("restarting the service")
	if _, err := fmt.Fprintln(conn, "restart"); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(patience))
	said, err := bufio.NewReader(conn).ReadString('\n')
	if said = strings.TrimSpace(said); said != "" {
		return errors.New(said)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !hub.Awaits(hub.TrayAddr()) {
		return errors.New("it did not come back")
	}
	return nil
}

// passing bounds the wait for a session moving to the new binary: it execs within a few
// milliseconds, and one that has not answered within that delay will not.
const passing = 5 * time.Second

// handedOver moves the running sessions to the binary just laid down: each execs onto itself and
// keeps its apps. None is taken at its word — the session joined afterwards states the version it
// carries, and that answer is what counts.
func handedOver(step func(string), version string) {
	living, err := hub.Sessions()
	if err != nil || len(living) == 0 {
		return
	}
	took := 0
	for _, session := range living {
		notApplied, err := handed(session.Config, version)
		if err != nil {
			step(filepath.Base(session.Config) + " keeps the old binary: " + firstLine(err.Error()))
			continue
		}
		took++
		// A new binary that refuses the file has not lost the session: it serves on the configuration
		// in force, and this is where whoever started the update is told.
		if notApplied != "" {
			step(filepath.Base(session.Config) + " took it, and keeps the configuration in force: " + notApplied)
		}
	}
	if took > 0 {
		step(fmt.Sprintf("%d session(s) took it, apps and all", took))
	}
}

// handed asks a session for the handover and waits to see it back, then returns what the returned
// session does not apply of its configuration. The move is bounded end to end: a hanging session
// does not hold up the update of the rest.
func handed(config, version string) (string, error) {
	type answer struct {
		notApplied string
		err        error
	}
	done := make(chan answer, 1)
	go func() {
		notApplied, err := handover(config, version)
		done <- answer{notApplied, err}
	}()
	select {
	case got := <-done:
		return got.notApplied, got.err
	case <-time.After(passing):
		return "", errors.New("it did not answer")
	}
}

func handover(config, version string) (string, error) {
	link, err := attach.Watch(config)
	if err != nil {
		return "", err
	}
	if err := link.Order("renew"); err != nil {
		link.Close()
		return "", err
	}
	// The link drops with the session's exec, and that is the acknowledgement. If it holds, the session
	// refused to hand over, and said so in its log.
	for range link.States() {
	}
	link.Close()
	return back(config, version)
}

// back joins the returned session, compares what it carries with what was just laid down, and
// returns what it does not apply of its configuration. Its socket never stopped accepting: what takes
// a moment is the session itself, rereading its configuration and adopting its apps.
func back(config, version string) (string, error) {
	err := errors.New("it did not come back")
	for range int(passing / (200 * time.Millisecond)) {
		var link *attach.Link
		if link, err = attach.Watch(config); err == nil {
			state, live := <-link.States()
			link.Close()
			switch {
			case !live:
				err = errors.New("it went away")
			case state.Version != version:
				err = errors.New("it came back as " + state.Version)
			default:
				return state.NotApplied, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", err
}

// install lays down the new binary, from where this one comes and where it is.
func install(step func(string), exe string) error {
	if build.Origin == "" {
		return fromRelease(step, exe)
	}
	return fromClone(step, exe)
}

// fromClone fetches the commits and rebuilds. build.sh builds and installs, as by hand: how to build
// this repository lives in that script and nowhere else. The destination is the binary being updated,
// full path: a moved binary, or one that no longer bears the name of the clone's branch, replaces
// itself instead of seeing the new one land beside it.
func fromClone(step func(string), exe string) error {
	if _, err := git("rev-parse", "@{u}"); err == nil {
		step("fetching the commits")
		if _, err := git("fetch", "--quiet"); err != nil {
			step("the repository is out of reach: building what the clone has")
		}
	}
	if fastForwards() {
		step("taking them")
		if out, err := git("merge", "--ff-only", "@{u}"); err != nil {
			return fmt.Errorf("the clone cannot take them: %w%s", err, said(firstLine(out)))
		}
	}
	step("building")
	out, err := runWith([]string{"DSVS_TARGET=" + exe}, build.Origin, "sh", "build.sh", "--install")
	if err != nil {
		return fmt.Errorf("build.sh: %w%s", err, said(lastLine(out)))
	}
	return nil
}

// buildable is the version the clone would build. The fetch is best effort: the clone answers
// offline and for free, and an unreachable repository leaves the question unanswered rather than
// answered uselessly — what is at hand is what counts then.
//
// A modified clone has no version that sets it apart: `git describe --dirty` marks every dirty state
// with the same suffix, so an update would be offered again and again and always end on "nothing new
// was installed". That is the only thing to say in that case.
func buildable() (string, error) {
	out, err := git("status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return "", fmt.Errorf("%s: %w%s", build.Origin, err, said(firstLine(out)))
	}
	if strings.TrimSpace(out) != "" {
		return "", fmt.Errorf("%s has uncommitted changes: commit them, or install by hand with ./build.sh --install", build.Origin)
	}
	_, _ = git("fetch", "--quiet")
	described := []string{"describe", "--tags", "--always"}
	if fastForwards() {
		described = append(described, "@{u}")
	}
	out, err = git(described...)
	if err != nil {
		return "", fmt.Errorf("%s: %w%s", build.Origin, err, said(firstLine(out)))
	}
	version := strings.TrimSpace(out)
	if version == build.Version {
		return "", nil
	}
	return version, nil
}

// fastForwards says the upstream is reachable by fast-forward: that is what makes it the version to
// take, and its absence makes it the tree here. Unpushed local commits, a branch without upstream, an
// unreachable repository — in all these cases the clone is built as it is, and there is nothing to
// fetch first.
func fastForwards() bool {
	_, err := git("merge-base", "--is-ancestor", "HEAD", "@{u}")
	return err == nil
}

// Repo is the GitHub repository of the releases: DSVS_REPO, or the tool's own.
func Repo() string {
	if named := os.Getenv("DSVS_REPO"); named != "" {
		return named
	}
	return "glimps-re/dsvs"
}

func git(args ...string) (string, error) {
	return runWith(nil, build.Origin, "git", args...)
}

// runWith runs a command in that directory, with these variables on top of its own, and returns what
// it wrote, output and errors mixed: it is an explanation to show, not an output to process.
func runWith(env []string, dir, name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	out, err := command.CombinedOutput()
	return string(out), err
}

// VersionOf asks a binary for its version, which is also the only way to know it starts. A binary
// that fails silently leaves only its status: that is the cause then. The timeout is what turns the
// question into an answer: rollback is decided here, and a binary hanging without it would leave the
// update stuck with .old laid down and nothing decided.
func VersionOf(exe string) (string, error) {
	waiting, done := context.WithTimeout(context.Background(), patience)
	defer done()
	out, err := exec.CommandContext(waiting, exe, "--version").CombinedOutput()
	switch {
	case waiting.Err() != nil:
		return "", fmt.Errorf("%s did not answer --version within %s", filepath.Base(exe), patience)
	case err != nil:
		return "", fmt.Errorf("%w%s", err, said(lastLine(string(out))))
	}
	return strings.TrimSpace(string(out)), nil
}

// said appends to a cause what the command wrote, when it wrote something. The output completes the
// error, it does not replace it: a command that fails before starting — a vanished clone, for
// instance — writes nothing at all, and "nothing" cannot be read.
func said(line string) string {
	if line == "" {
		return ""
	}
	return ": " + line
}

// copyTo writes a copy of this file, executable like it: the new one only makes sense if the old one
// can take its place back.
func copyTo(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	copied, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(copied, source); err != nil {
		copied.Close()
		return err
	}
	return copied.Close()
}

func firstLine(said string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(said), "\n")
	return line
}

// lastLine keeps, of a talkative command, the line that says why it failed: the last one.
func lastLine(said string) string {
	lines := strings.Split(strings.TrimSpace(said), "\n")
	return lines[len(lines)-1]
}
