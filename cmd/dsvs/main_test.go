package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"dsvs/internal/build"
)

// The role is the decision point of the command: set up wrong, it had made a role of the table
// unreachable. A flag names the role, wherever it sits in the table; none leaves the default role, the
// one that reads the command line.
func TestOnlyPicksTheRoleAsked(t *testing.T) {
	for _, tc := range []struct {
		what   string
		asked  []string
		chosen string
	}{
		{"no flag", nil, "default"},
		{"the first of the table", []string{"hub"}, "hub"},
		{"the last of the table", []string{"session"}, "session"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			chosen, err := only(asking(tc.asked...), named("default"))
			if err != nil {
				t.Fatalf("only = %v, want a role", err)
			}
			if got := chosen(); got.Error() != tc.chosen {
				t.Errorf("role %v; want %s", got, tc.chosen)
			}
		})
	}
}

// Two roles at once are refused by naming them, where the order of the lines would decide silently.
func TestOnlyRefusesTwoRolesByNamingThem(t *testing.T) {
	chosen, err := only(asking("hub", "session"), named("default"))
	if chosen != nil {
		t.Error("only returned a role for two flags")
	}
	if err == nil || !strings.Contains(err.Error(), "--hub") || !strings.Contains(err.Error(), "--session") {
		t.Errorf("error = %v; want both flags named", err)
	}
}

// asking returns a table of roles reduced to their name, the given ones asked for. Each returns its
// name as an error: that is what tells which one only chose.
func asking(asked ...string) []role {
	var roles []role
	for _, name := range []string{"hub", "tray", "session"} {
		roles = append(roles, role{name, slices.Contains(asked, name), named(name)})
	}
	return roles
}

func named(name string) func() error {
	return func() error { return errors.New(name) }
}

// The two ways of choosing the session's port: a shift applies to the one the file declares, a typed
// port is absolute and is not shifted. Either one forces the port, which makes it hold across reloads;
// without a choice, the file has the last word at every reread.
func TestLoadChoosesThePortOfTheSession(t *testing.T) {
	path := declaring(t, 4700)
	for _, tc := range []struct {
		what        string
		port, shift int
		wanted      int
		forced      int
	}{
		{"no choice", 0, 0, 4700, 0},
		{"a shift", 0, 100, 4800, 4800},
		{"a typed port", 4999, 0, 4999, 4999},
		{"a typed port despite a shift", 4999, 100, 4999, 4999},
	} {
		t.Run(tc.what, func(t *testing.T) {
			cfg, err := load(path, tc.port, tc.shift, nil)
			if err != nil {
				t.Fatalf("load = %v", err)
			}
			if cfg.Port != tc.wanted {
				t.Errorf("port = %d; want %d", cfg.Port, tc.wanted)
			}
			if cfg.ForcedPort != tc.forced {
				t.Errorf("forced port = %d; want %d", cfg.ForcedPort, tc.forced)
			}
		})
	}
}

// The shift lives in the environment, which the service's unit sets: that is where the flag takes its
// default from, and a value that is not an integer shifts nothing.
func TestPortShiftComesFromTheEnvironment(t *testing.T) {
	for value, wanted := range map[string]int{"": 0, "100": 100, "-100": -100, "hundred": 0} {
		t.Setenv("DSVS_PORT_SHIFT", value)
		if got := portShift(); got != wanted {
			t.Errorf("DSVS_PORT_SHIFT=%q gives %d; want %d", value, got, wanted)
		}
	}
}

// The shift crosses the environment when the service's unit sets it; typed by hand, it only reaches the
// session being started through its flags, like a typed port.
func TestShapeCarriesThePortShiftToTheSession(t *testing.T) {
	for _, tc := range []struct {
		what        string
		port, shift int
		wanted      []string
	}{
		{"no choice", 0, 0, nil},
		{"a shift", 0, 100, []string{"--port-shift", "100"}},
		{"a typed port", 4999, 0, []string{"--port", "4999"}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			s := shape{port: tc.port, shift: tc.shift, net: "auto"}
			if got := s.args(); !slices.Equal(got, tc.wanted) {
				t.Errorf("args = %v; want %v", got, tc.wanted)
			}
		})
	}
}

// declaring writes a configuration that declares this port, and returns its path.
func declaring(t *testing.T, port int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dsvs.json")
	config := fmt.Sprintf(`{"port": %d, "ingress": [{"host": "shop.localhost",
		"routes": [{"path": "/", "app": "front"}]}],
		"apps": [{"name": "front", "command": ["true", "{port}"]}]}`, port)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReleaseOfAVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		tag     string
		exact   bool
		fails   bool
	}{
		{"v0.5.0", "v0.5.0", true, false},
		{"v0.4.2-6-g4009923", "v0.4.2", false, false},
		{"v0.4.2-dirty", "v0.4.2", false, false},
		{"v0.4.2-6-g4009923-dirty", "v0.4.2", false, false},
		{"dev", "", false, true},
		{"4009923", "", false, true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			tag, exact, err := release(tc.version)
			if (err != nil) != tc.fails || tag != tc.tag || exact != tc.exact {
				t.Errorf("release = (%q, %v, %v); want (%q, %v, error %v)",
					tag, exact, err, tc.tag, tc.exact, tc.fails)
			}
		})
	}
}

func TestTypesNameTheRepositoryAndTheTagOfTheBinary(t *testing.T) {
	t.Setenv("DSVS_REPO", "a/b")
	was := build.Version
	build.Version = "v0.4.2-6-g4009923"
	t.Cleanup(func() { build.Version = was })

	var out, said bytes.Buffer
	if err := writeTypes(&out, &said); err != nil {
		t.Fatal(err)
	}
	if want := "git+ssh://git@github.com/a/b.git#v0.4.2&path:/types\n"; out.String() != want {
		t.Errorf("specifier %q; want %q", out.String(), want)
	}
	if !strings.Contains(said.String(), "ahead of its last tag") {
		t.Errorf("stderr %q does not say the binary is ahead", said.String())
	}
}
