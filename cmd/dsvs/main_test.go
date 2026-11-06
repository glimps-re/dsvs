package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"dsvs/internal/build"
	"dsvs/internal/config"
	"dsvs/internal/projects"
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
			_, cfg, err := load(config.Load, path, tc.port, tc.shift, nil, config.Input{})
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

// A project is only remembered if its session started: a refused start — a port another one holds, an
// invalid form — remembered it anyway, and the panel would have offered it cold.
func TestARefusedSessionDoesNotRememberItsProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.dsvs.ts")
	if err := os.WriteFile(path, []byte(`throw new Error("refused");`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(&shape{config: path}, false, filepath.Join(t.TempDir(), "absent"), "asked for by hand", nil, nil); err == nil {
		t.Fatal("the session started on a configuration that throws")
	}
	if got := projects.List(); len(got) != 0 {
		t.Errorf("remembered projects = %v, want none", got)
	}
}

// An unreachable gatekeeper does not hold the session back: it starts without an observation, and the
// log says why.
func TestTheSessionStartsWhenTheNetworkCannotBeObserved(t *testing.T) {
	var said []string
	observed, _ := observe(context.Background(), filepath.Join(t.TempDir(), "absent"), func(line string) {
		said = append(said, line)
	})

	if observed != nil {
		t.Errorf("observation = %+v, want none", observed)
	}
	if len(said) != 1 || !strings.HasPrefix(said[0], "-- network not observed: ") {
		t.Errorf("log = %q, want a \"network not observed\" line", said)
	}
}

// What the panel lists cold, status writes: as text and as JSON, and without a project it says so
// rather than staying silent.
func TestStatusWritesTheRememberedProjects(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	var none bytes.Buffer
	if err := status(&none, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none.String(), "  remembered: none\n") {
		t.Errorf("without a project, status writes %q", none.String())
	}
	none.Reset()
	if err := status(&none, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(none.String(), "remembered") {
		t.Errorf("sans projet, le JSON porte %q", none.String())
	}

	config := filepath.Join(t.TempDir(), "dsvs.json")
	if err := os.WriteFile(config, []byte(`{"apps":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := projects.Remember(config); err != nil {
		t.Fatal(err)
	}

	var text bytes.Buffer
	if err := status(&text, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "  remembered  "+config+"\n") {
		t.Errorf("status writes %q, want the remembered project", text.String())
	}

	var asJSON bytes.Buffer
	if err := status(&asJSON, true); err != nil {
		t.Fatal(err)
	}
	var decoded struct{ Remembered []string }
	if err := json.Unmarshal(asJSON.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoded.Remembered, []string{config}) {
		t.Errorf("remembered = %v, want [%s]", decoded.Remembered, config)
	}
}

// Remembering then forgetting the project here goes through the projects file, and each gesture says
// what it did — including when there was nothing to do, without that being an error.
func TestRememberThenForgetTheProjectHere(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	config := filepath.Join(dir, "dsvs.json")
	if err := os.WriteFile(config, []byte(`{"apps":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	below := filepath.Join(dir, "sub")
	if err := os.Mkdir(below, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(below)
	// Under /tmp, the resolved path may go through a link: expect the one resolve returns.
	abs, err := resolve("")
	if err != nil {
		t.Fatal(err)
	}

	said := func(run func(io.Writer, *shape, []string) error, s *shape) (string, error) {
		var out bytes.Buffer
		err := run(&out, s, nil)
		return out.String(), err
	}

	if got, err := said(remembering, &shape{}); err != nil || got != "remembered "+abs+"\n" {
		t.Errorf("remember = %q, %v", got, err)
	}
	if got := projects.List(); !slices.Equal(got, []string{abs}) {
		t.Errorf("projects = %v, want [%s]", got, abs)
	}
	if got, err := said(remembering, &shape{}); err != nil || got != abs+" is already remembered\n" {
		t.Errorf("second remember = %q, %v", got, err)
	}

	if got, err := said(forgetting, &shape{}); err != nil || got != "forgot "+abs+"\n" {
		t.Errorf("forget = %q, %v", got, err)
	}
	if got := projects.List(); len(got) != 0 {
		t.Errorf("projects = %v, want none", got)
	}
	if got, err := said(forgetting, &shape{}); err != nil || !strings.Contains(got, abs+" was not remembered") {
		t.Errorf("second forget = %q, %v", got, err)
	}

	if _, err := said(remembering, &shape{config: dir}); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("remember of a directory = %v", err)
	}
	if got, err := said(forgetting, &shape{config: "/does/not/exist/dsvs.json"}); err != nil {
		t.Errorf("forget of a vanished project = %q, %v", got, err)
	}
}
