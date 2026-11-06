package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsvs/internal/build"
)

// The version that would be taken is the one the clone would build: the same one says there is
// nothing to do, another one says what to take. Without upstream or network, the question is settled
// with what is at hand.
func TestNewerComparesWithWhatTheCloneWouldBuild(t *testing.T) {
	clone := commits(t)
	from(t, clone, described(t, clone))

	if newer, err := Newer(); err != nil || newer != "" {
		t.Errorf("Newer = %q, %v; want nothing to take", newer, err)
	}

	commit(t, clone, "again")
	wanted := described(t, clone)
	if newer, err := Newer(); err != nil || newer != wanted {
		t.Errorf("Newer = %q, %v; want %q", newer, err, wanted)
	}
}

// A modified clone does not update: what it would build would carry the same version whatever
// changes, and the update would be offered again and again. Saying so is the only honest answer.
func TestNewerRefusesADirtyClone(t *testing.T) {
	clone := commits(t)
	from(t, clone, described(t, clone))
	if err := os.WriteFile(filepath.Join(clone, "first"), []byte("modified"), 0o600); err != nil {
		t.Fatal(err)
	}

	newer, err := Newer()
	if newer != "" {
		t.Errorf("Newer = %q; a dirty clone has nothing to offer", newer)
	}
	if err == nil || !strings.Contains(err.Error(), clone) {
		t.Errorf("err = %v; want a reason naming %s", err, clone)
	}
}

// A vanished clone fails before writing anything: the cause is in the command's status, not in its
// output, and an empty error cannot be read.
func TestNewerSaysWhenTheCloneIsGone(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	from(t, gone, "v0.0.1")

	newer, err := Newer()
	if newer != "" {
		t.Errorf("Newer = %q; a vanished clone has nothing to offer", newer)
	}
	if err == nil || strings.TrimPrefix(err.Error(), gone+": ") == "" {
		t.Errorf("err = %q; want a reason, not just the path", err)
	}
}

// Rollback is the safety net the README promises: a new binary that does not start gives its place
// back to the old one, and the error says both.
func TestApplyPutsTheOldBinaryBackWhenTheNewOneDoesNotRun(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "dsvs")
	binary(t, exe, "v-old")

	clone := commits(t)
	// This build.sh installs a binary that does not start, exactly where the real one puts it.
	script(t, clone, "printf 'not a binary' > \"$DSVS_TARGET\"\nchmod 755 \"$DSVS_TARGET\"\n")
	from(t, clone, "v-old")

	err := apply(exe, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "the old one is back") {
		t.Fatalf("Apply = %v; want the rollback reported", err)
	}
	if out, err := runWith(nil, "", exe, "--version"); err != nil || strings.TrimSpace(out) != "v-old" {
		t.Errorf("the binary in place returns %q, %v; want the old one", out, err)
	}
}

// A binary that does not start must say why: that finding is what triggers the rollback, and
// "nothing" cannot be read.
func TestVersionOfSaysWhyABinaryDoesNotRun(t *testing.T) {
	if _, err := VersionOf(filepath.Join(t.TempDir(), "never-built")); err == nil {
		t.Error("a missing binary returned a version")
	}
	_, err := VersionOf("/bin/false")
	if err == nil || strings.TrimSpace(err.Error()) == "" {
		t.Errorf("err = %v, want a reason", err)
	}
}

// commits returns a disposable clone with one commit.
func commits(t *testing.T) string {
	t.Helper()
	clone := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@local"},
		{"config", "user.name", "test"},
		// A disposable clone inherits the global configuration: that of someone who signs their
		// commits would leave the test waiting on a key prompt nobody will see.
		{"config", "commit.gpgsign", "false"},
	} {
		if out, err := runWith(nil, clone, "git", args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	commit(t, clone, "first")
	return clone
}

func commit(t *testing.T, clone, what string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(clone, what), []byte(what), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runWith(nil, clone, "git", "add", "-A"); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if out, err := runWith(nil, clone, "git", "commit", "--quiet", "-m", what); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
}

// binary lays down a binary that states this version, and nothing else: that is all the update asks
// of it.
func binary(t *testing.T, exe, version string) {
	t.Helper()
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho "+version+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// script gives this clone the build.sh the update will run.
func script(t *testing.T, clone, what string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(clone, "build.sh"), []byte("#!/bin/sh\nset -eu\n"+what), 0o755); err != nil {
		t.Fatal(err)
	}
}

func described(t *testing.T, clone string) string {
	t.Helper()
	out, err := runWith(nil, clone, "git", "describe", "--tags", "--always", "--dirty")
	if err != nil {
		t.Fatalf("git describe: %v %s", err, out)
	}
	return strings.TrimSpace(out)
}

// from makes this clone the binary's provenance, or the releases when there is no clone: the build
// sets it, and the test replaces it for its duration.
func from(t *testing.T, clone, version string) {
	t.Helper()
	origin, was := build.Origin, build.Version
	t.Cleanup(func() { build.Origin, build.Version = origin, was })
	build.Origin, build.Version = clone, version
}
