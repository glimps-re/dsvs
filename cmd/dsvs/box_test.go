package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The service is born from the systemd manager, whose environment has neither node nor pnpm: it
// takes the one of the user's login shell, with what the rc files put in it.
func TestLoginEnvTakesWhatTheShellGives(t *testing.T) {
	t.Setenv("SHELL", shellScript(t, `PATH=/opt/rc/bin:/usr/bin:/bin
FROM_RC=1
export PATH FROM_RC
exec /bin/sh -c "$2"`))

	env, err := loginEnv(10 * time.Second)
	if err != nil {
		t.Fatalf("loginEnv = %v, want an environment", err)
	}
	for _, wanted := range []string{"FROM_RC=1", "PATH=/opt/rc/bin:/usr/bin:/bin"} {
		if !slices.Contains(env, wanted) {
			t.Errorf("environment without %s: %v", wanted, env)
		}
	}
}

// A shell that fails to return its environment leaves the manager's, with the reason.
func TestLoginEnvRefusesWhatItCannotTrust(t *testing.T) {
	for _, tc := range []struct{ what, script string }{
		{"the shell exits with an error", "exit 1"},
		{"the shell gives no PATH", `exec env -i NOTHING=here env -0 >&3`},
	} {
		t.Run(tc.what, func(t *testing.T) {
			t.Setenv("SHELL", shellScript(t, tc.script))
			if env, err := loginEnv(10 * time.Second); err == nil {
				t.Errorf("loginEnv = %v, want an error", env)
			}
		})
	}

	t.Run("the shell does not exist", func(t *testing.T) {
		t.Setenv("SHELL", filepath.Join(t.TempDir(), "absent"))
		if env, err := loginEnv(10 * time.Second); err == nil {
			t.Errorf("loginEnv = %v, want an error", env)
		}
	})
}

// An rc waiting for input does not hold the icon back: the deadline decides, and returns at once.
func TestLoginEnvGivesUpOnASlowShell(t *testing.T) {
	t.Setenv("SHELL", shellScript(t, `sleep 30
exec /bin/sh -c "$2"`))

	started := time.Now()
	env, err := loginEnv(200 * time.Millisecond)
	if err == nil {
		t.Fatalf("loginEnv = %v, want an error", env)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Errorf("loginEnv waited %s, beyond its deadline", waited)
	}
}

// shellScript returns the path of a script playing the user's shell: it receives -ilc then the
// command, like bash.
func shellScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
