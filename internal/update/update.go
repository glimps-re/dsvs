package update

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// patience bounds the wait for a repository or a binary: the clone answers at once, GitHub not
// always, and a machine without a network never does.
const patience = 20 * time.Second

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

// lastLine keeps, of a talkative command, the line that says why it failed: the last one.
func lastLine(said string) string {
	lines := strings.Split(strings.TrimSpace(said), "\n")
	return lines[len(lines)-1]
}
