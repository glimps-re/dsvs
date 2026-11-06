package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"dsvs/internal/attach"
	"dsvs/internal/build"
	"dsvs/internal/config"
	"dsvs/internal/core"
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

func TestSetTakesNameValuePairs(t *testing.T) {
	for _, line := range [][]string{{"set"}, {"set", "env"}} {
		err := dispatch(&shape{}, line)
		if err == nil || !strings.Contains(err.Error(), "set takes name=value") {
			t.Errorf("dsvs %s: error %v, want \"set takes name=value\"", strings.Join(line, " "), err)
		}
	}
}

func TestStateSaysTheParametersAndTheNetwork(t *testing.T) {
	state := attach.State{
		Config: "/p/config.dsvs.ts",
		Port:   4380,
		Params: []config.Field{
			{Name: "env", Type: "choice", Choices: []string{"local", "staging"}, Value: "staging", From: "default"},
			{Name: "slow", Type: "flag", Choices: []string{"false", "true"}, Value: "false", From: "default"},
		},
		Observed: &config.Observed{Links: []config.Link{{Name: "tun0"}, {Name: "wlan0"}}, Exits: []config.Exit{{Via: "198.51.100.2", Dev: "wlan0"}}},
	}
	var out bytes.Buffer
	if err := writeState(&out, state); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"params      env=staging (local staging)  slow=false",
		"network     wlan0 via 198.51.100.2, tun0",
	}
	if got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n"); !slices.Equal(got[len(got)-2:], want) {
		t.Errorf("end of the state %q; want %q", got[len(got)-2:], want)
	}

	// A hidden field does not exist here; where a value comes from, what yields, what is inert and what
	// is wrong are said, each on its own line.
	state.Params = []config.Field{
		{Name: "env", Type: "choice", Choices: []string{"local", "staging"}, Value: "staging", From: "kept"},
		{Name: "api.deployment", Type: "choice", Choices: []string{"alpha", "beta"}, Value: "beta", From: "network",
			DisabledChoices: map[string]string{"alpha": "alpha is retired"}},
		{Name: "api.mirror", Type: "flag", Choices: []string{"false", "true"}, Value: "false", From: "default", Disabled: true,
			Reasons: []string{"the mirror is only reachable from the office network"}},
		{Name: "locale", Type: "choice", Choices: []string{"en", "fr"}, Value: "fr", From: "session", Errors: []string{"beta has no fr build"}},
		{Name: "delay", Type: "number", Value: "0", From: "default", Hidden: true},
		{Name: "branch", Type: "text", Value: "", From: "default"},
		{Name: "discount", Type: "number", Value: "15", From: "default", Yielded: &config.Yield{Value: "20", From: "kept", Errors: []string{"at most 15"}}},
	}
	state.Errors, state.NotApplied = []string{"the form as a whole"}, "locale: beta has no fr build; the form as a whole"
	state.Notices = []string{"delay is hidden, yet apps.api.command[2] depends on it"}
	out.Reset()
	if err := writeState(&out, state); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"params      env=staging (local staging)  api.deployment=beta (alpha beta)  api.mirror=false  locale=fr (en fr)  branch=  discount=15",
		"from        env: kept  api.deployment: network  locale: session",
		"yielded     discount=20 no longer holds (at most 15): discount=15 until it does",
		"notice      delay is hidden, yet apps.api.command[2] depends on it",
		"disabled    api.deployment: alpha is disabled here: alpha is retired  api.mirror: the mirror is only reachable from the office network",
		"invalid     locale: beta has no fr build  the form as a whole",
		"network     wlan0 via 198.51.100.2, tun0",
		"not applied locale: beta has no fr build; the form as a whole (" + build.Name() + " logs)",
	}
	if got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n"); !slices.Equal(got[len(got)-len(want):], want) {
		t.Errorf("end of the state:\n%s\nwant:\n%s", strings.Join(got[len(got)-len(want):], "\n"), strings.Join(want, "\n"))
	}

	state.Params, state.Errors, state.Notices, state.Observed, state.NotApplied = nil, nil, nil, nil, ""
	out.Reset()
	if err := writeState(&out, state); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); strings.Contains(text, "params") || !strings.HasSuffix(text, "network     not observed\n") {
		t.Errorf("without a parameter or an observation, the state says:\n%s", text)
	}
}

// An app that starts without listening on its address says so below the table of apps, where its
// column would not have the room.
func TestStateSaysWhereAnAppListensInstead(t *testing.T) {
	state := attach.State{Config: "/p/config.dsvs.ts", Port: 4380, Apps: []attach.App{
		{Name: "front", Status: "starting", PID: 41, Astray: "listens on 127.0.0.1:5173, not on 10.77.0.3:4123"},
		{Name: "api", Status: "ready", Ready: true, PID: 42},
	}}
	var out bytes.Buffer
	if err := writeState(&out, state); err != nil {
		t.Fatal(err)
	}
	if line := "front listens on 127.0.0.1:5173, not on 10.77.0.3:4123\n"; !strings.Contains(out.String(), "api    ready     yes    42\n"+line) {
		t.Errorf("the state says:\n%s\nwant, below the table: %s", out.String(), line)
	}
}

// startBody is a form in which two memories, each valid on its own, form a pair the form refuses.
const startBody = `import { config, form, keep, network, validateTree } from "dsvs";
const params = form({ env: ["local", "staging"], staging: ["office", "public"], locale: ["en", "fr"] }, (p) => {
  keep(p.env);
  keep(p.locale);
  network(p.staging);
  validateTree(p, ({ valueOf }) => valueOf(p.locale) === "fr" && valueOf(p.staging) === "public" ? { field: p.locale, message: "public has no fr build" } : null);
});
export default config(params, ({ env, staging, locale }) => ({
  port: 4700,
  ingress: [{ host: "shop.localhost", routes: [{ path: "/", app: "front" }, { path: "/api", proxy: { target: "https://" + env + "-" + staging + "-" + locale + ".example" } }] }],
  apps: [{ name: "front", command: ["serve", "{port}"] }],
}));`

// What dsvs config writes reads back as is: a parametrized configuration therefore comes out without its
// parameters, built from the values asked for, on top of what a session opened here would have.
func TestConfigOfAParametrizedFileReadsBack(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.dsvs.ts")
	if err := os.WriteFile(path, []byte(startBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := projects.Keep(path, map[string]string{"env": "staging"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := printConfig(&out, &shape{config: path}, []string{"staging=public"}, nil, false); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(dir, "snapshot.dsvs.json")
	if err := os.WriteFile(snapshot, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, cfg, err := config.Load(snapshot, nil, config.Input{})
	if err != nil {
		t.Fatalf("the output of dsvs config does not read back: %v", err)
	}
	if in, _ := cfg.Ingress("shop.localhost"); !strings.Contains(out.String(), "https://staging-public-en.example") || len(in.Routes) != 2 {
		t.Errorf("the reread does not carry the staging target:\n%s", out.String())
	}

	for pair, refused := range map[string]string{
		"env=nope":  `env: "nope" is not one of local, staging`,
		"locale=fr": "locale: public has no fr build",
	} {
		err = printConfig(&out, &shape{config: path}, []string{"staging=public", pair}, nil, false)
		if err == nil || !strings.HasSuffix(err.Error(), refused) {
			t.Errorf("%s: error %v, want %q", pair, err, refused)
		}
	}
}

// networkBody chooses its target according to the network it is given: a tunnel opens the vpn choice,
// and offline the target is the fallback one.
const networkBody = `import { choices, config, form, network } from "dsvs";
const params = form({ staging: ["office", "vpn", "public"] }, (p, observed) => {
  network(p.staging);
  const tunnel = observed?.links.some((link) => link.type === "tun") ?? false;
  choices(p.staging, () => (tunnel ? ["office", "vpn", "public"] : ["office", "public"]));
});
export default config(params, ({ staging }, observed) => ({
  port: 4700,
  ingress: [{ host: "shop.localhost", routes: [{ path: "/", app: "front" }, { path: "/api", proxy: { target: observed ? "https://" + staging + ".example" : "https://offline.example" } }] }],
  apps: [{ name: "front", command: ["serve", "{port}"] }],
}));`

// dsvs config answers for the network described to it, with what the project keeps there, and --params
// writes the form these values give, refused or not.
func TestConfigAnswersForTheNetworkItIsGiven(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.dsvs.ts")
	if err := os.WriteFile(path, []byte(networkBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := projects.Retain(path, "eth0 via 198.51.100.1", map[string]string{"staging": "vpn"}); err != nil {
		t.Fatal(err)
	}
	described := func(text string) *config.Observed {
		t.Helper()
		file := filepath.Join(dir, "observed.json")
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		seen, err := networkFor(file)
		if err != nil {
			t.Fatal(err)
		}
		return seen
	}
	tunnel := described(`{"links":[{"name":"tun0","type":"tun"},{"name":"eth0","type":"ether"}],"exits":[{"via":"198.51.100.1","dev":"eth0"}]}`)
	plain := described(`{"links":[{"name":"eth0","type":"ether"}],"exits":[{"via":"198.51.100.2","dev":"eth0"}]}`)
	here := &shape{config: path}

	for _, network := range []struct {
		name   string
		seen   *config.Observed
		target string
	}{
		{"tunnel", tunnel, "https://vpn.example"},
		{"plain", plain, "https://office.example"},
		{"null", described("null"), "https://offline.example"},
	} {
		var out bytes.Buffer
		if err := printConfig(&out, here, nil, network.seen, false); err != nil {
			t.Fatalf("%s: %v", network.name, err)
		}
		if !strings.Contains(out.String(), `"target": "`+network.target+`"`) {
			t.Errorf("%s: want the target %s:\n%s", network.name, network.target, out.String())
		}
	}

	var out bytes.Buffer
	if err := printConfig(&out, here, nil, tunnel, true); err != nil {
		t.Fatal(err)
	}
	var form config.Form
	if err := json.Unmarshal(out.Bytes(), &form); err != nil {
		t.Fatalf("--params does not write a form: %v\n%s", err, out.String())
	}
	want := config.Field{Name: "staging", Type: "choice", Choices: []string{"office", "vpn", "public"}, Value: "vpn", From: "network", Network: true}
	if len(form.Fields) != 1 || !reflect.DeepEqual(form.Fields[0], want) {
		t.Errorf("form %+v, want %+v", form.Fields, want)
	}

	out.Reset()
	err := printConfig(&out, here, []string{"staging=vpn"}, plain, true)
	if err == nil || !strings.HasSuffix(err.Error(), `staging: "vpn" is not one of office, public`) {
		t.Errorf("without a tunnel, staging=vpn gives %v", err)
	}
	if !strings.Contains(out.String(), `"value": "office"`) {
		t.Errorf("the refused form is not written:\n%s", out.String())
	}

	if _, err := networkFor(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing file gives a network")
	}
	file := filepath.Join(dir, "state.json")
	if err := os.WriteFile(file, []byte(`{"config":"/p","observed":null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := networkFor(file); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("a whole state given for its network gives %v", err)
	}
}

// A session opens on what the project and its network keep. Two memories forming a refused pair do not
// prevent it from starting: the one the rule designates yields. run takes values beside its names, and
// asks for them at the first read; a handover brings its own.
func TestASessionStartsOnWhatItsProjectKeeps(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.dsvs.ts")
	if err := os.WriteFile(path, []byte(startBody), 0o644); err != nil {
		t.Fatal(err)
	}
	on := func(gateway string) *config.Observed {
		return &config.Observed{Links: []config.Link{{Name: "eth0"}}, Exits: []config.Exit{{Via: gateway, Dev: "eth0"}}}
	}
	a, b := on("198.51.100.1"), on("198.51.100.2")
	if err := projects.Keep(path, map[string]string{"env": "staging", "locale": "fr"}); err != nil {
		t.Fatal(err)
	}
	if err := projects.Retain(path, b.Network(), map[string]string{"staging": "public"}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name     string
		observed *config.Observed
		taken    *core.Handover
		asked    map[string]string
		target   string
		refused  string
	}{
		{"on A", a, nil, nil, "https://staging-office-fr.example", ""},
		{"offline", nil, nil, nil, "https://staging-office-fr.example", ""},
		{"on B", b, nil, nil, "https://staging-public-en.example", ""},
		{"on B, asking", b, nil, map[string]string{"locale": "en"}, "https://staging-public-en.example", ""},
		{"on B, asking what it refuses", b, nil, map[string]string{"staging": "nope"}, "", `"nope" is not one of office, public`},
		{"a handover on B", b, &core.Handover{Params: map[string]string{"locale": "en"}}, nil, "https://staging-public-en.example", ""},
	} {
		_, cfg, err := opened(path, &shape{}, c.observed, c.taken, c.asked)
		switch {
		case c.refused != "" && (err == nil || !strings.Contains(err.Error(), c.refused)):
			t.Errorf("%s: %v, want a refusal that says %q", c.name, err, c.refused)
		case c.refused == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.refused == "":
			if in, _ := cfg.Ingress("shop.localhost"); in.Routes[0].Proxy.URL != c.target {
				t.Errorf("%s: /api leads to %q, want %q", c.name, in.Routes[0].Proxy.URL, c.target)
			}
		}
	}

	// A handover resumes from the answer that built the configuration in effect, even when the file no
	// longer reads, and the session's port applies to it as to a read.
	_, inForce, err := opened(path, &shape{}, a, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("export default {,};"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, carried, err := opened(path, &shape{port: 4800}, a, &core.Handover{Answer: inForce.Answer}, nil)
	if in, _ := carried.Ingress("shop.localhost"); err != nil || in.Routes[0].Proxy.URL != "https://staging-office-fr.example" || carried.Port != 4800 {
		t.Errorf("a handover on an unreadable file: %v, port %d, configuration %+v", err, carried.Port, in)
	}

	names, asked, err := namesAndValues([]string{"all", "locale=en", "api"})
	if err != nil || !slices.Equal(names, []string{"all", "api"}) || !maps.Equal(asked, map[string]string{"locale": "en"}) {
		t.Errorf("run all locale=en api: names %v, values %v, %v", names, asked, err)
	}
}

// A session that does not start says why once. Outside a systemd box, its stderr is that of the
// terminal that started it: repeating the reason the client just read on its socket wrote it there
// twice. It therefore exits on errSaid, which die does not write.
func TestARefusedSessionSaysWhyOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.dsvs.ts")
	if err := os.WriteFile(path, []byte(`throw new Error("discount: at most 30");`), 0o644); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() {
		ended <- run(&shape{config: path}, true, filepath.Join(t.TempDir(), "absent"), "asked for by hand", nil, nil)
	}()

	if _, err := attach.Ensure(path, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "discount: at most 30") {
		t.Fatalf("the client read %v, want the session's reason", err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, errSaid) {
			t.Errorf("the session exits on %v, which it would repeat on stderr", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session stayed after saying its reason")
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
	for _, empty := range []string{`"sessions": []`, `"remembered": []`, `"peers": []`} {
		if !strings.Contains(none.String(), empty) {
			t.Errorf("without a project or a session, the JSON should carry %s: %q", empty, none.String())
		}
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

// dsvs set writes the state its gesture produced, even when the session pushed another one between the
// opening of its link and the gesture: the one of a network change it was replaying at the same moment.
// A heavy state — two thousand choices — takes a few milliseconds to arrive, and the gesture's "ok" none:
// reading the next pushed state then returned, every time, the network's one, from before the gesture.
func TestSetWritesTheStateItsGestureMade(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.dsvs.ts")
	body := `import { config, form } from "dsvs";
const heavy = Array.from({ length: 2000 }, (_, i) => "choice-" + i);
export default config(form({ env: ["local", "staging"], heavy: [heavy[0], ...heavy.slice(1)] }), ({ env }) => ({
  port: 4399,
  ingress: [{ host: "shop.localhost", routes: [{ path: "/", app: "sleeper" }] }],
  apps: [{ name: "sleeper", command: ["sh", "-c", "sleep 30", env, "{port}"] }],
}));`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := core.NewSession(form, *cfg, core.Loopback{}, closedFront{}, func(*core.Session) core.Gateway { return idleGateway{} })
	ctx, stop := context.WithCancel(context.Background())
	running := make(chan struct{})
	go func() {
		defer close(running)
		session.Run(ctx, nil)
	}()
	server, err := attach.Bind(path)
	if err != nil {
		t.Fatal(err)
	}
	server.Ready(ctx, session, stop)
	t.Cleanup(func() {
		stop()
		server.Close()
		<-running
	})

	for i, value := range []string{"staging", "local", "staging", "local", "staging"} {
		out, err := os.Create(filepath.Join(t.TempDir(), "state.txt"))
		if err != nil {
			t.Fatal(err)
		}
		// The network changes, and the session replays it: the gesture waits for the end of this
		// replay, which pushes the state from before it.
		seen := &config.Observed{Links: []config.Link{{Name: fmt.Sprintf("eth%d", i)}}}
		replayed := make(chan struct{})
		go func() {
			defer close(replayed)
			session.Observe(seen)
		}()
		for !reflect.DeepEqual(session.Observed(), seen) {
			runtime.Gosched()
		}
		stdout := os.Stdout
		os.Stdout = out
		err = setting(&shape{config: path}, []string{"env=" + value})
		os.Stdout = stdout
		<-replayed
		written, _ := os.ReadFile(out.Name())
		out.Close()
		if want := "params      env=" + value + " "; err != nil || !strings.Contains(string(written), want) {
			t.Fatalf("set #%d env=%s: %v, it writes\n%s", i+1, value, err, paramsOf(string(written)))
		}
	}
}

// An order the session refuses is a command that fails, with the session's reason: dsvs stop on a name
// it does not know answered with its state and a zero code, and the refusal only showed in its log.
func TestAnOrderTheSessionRefusesFails(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	cfg := config.Config{Dir: dir, Path: filepath.Join(dir, "config.dsvs.json"), Port: 4399,
		Apps: []config.App{{Name: "sleeper", Command: []string{"sh", "-c", "sleep 30"}, Dir: dir}}}
	session := core.NewSession(config.Form{}, cfg, core.Loopback{}, closedFront{}, func(*core.Session) core.Gateway { return idleGateway{} })
	ctx, stop := context.WithCancel(context.Background())
	running := make(chan struct{})
	go func() {
		defer close(running)
		session.Run(ctx, nil)
	}()
	server, err := attach.Bind(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	server.Ready(ctx, session, stop)
	t.Cleanup(func() {
		stop()
		server.Close()
		<-running
	})

	for _, verb := range []string{"stop", "restart"} {
		err := order(&shape{config: cfg.Path}, verb, []string{"ghost"})
		if err == nil || !strings.Contains(err.Error(), `"ghost"`) {
			t.Errorf("dsvs %s ghost = %v, want the session's refusal", verb, err)
		}
	}
}

// paramsOf returns the params line of a written state: the rest, two thousand choices included, would
// drown the failure.
func paramsOf(written string) string {
	for _, line := range strings.Split(written, "\n") {
		if strings.HasPrefix(line, "params") {
			return line[:min(len(line), 120)]
		}
	}
	return written[:min(len(written), 120)]
}

type closedFront struct{}

func (closedFront) Listen(int) (net.Listener, error) {
	return nil, errors.New("no port in this test")
}
func (closedFront) Hosts([]string) error { return nil }

type idleGateway struct{}

func (idleGateway) Start(context.Context) error { return nil }
func (idleGateway) Stop(context.Context) error  { return nil }
func (idleGateway) Stats(string) (int, int)     { return 0, 0 }

// dsvs config --session says what the session has in effect: without a session here, it is refused with
// the gesture to make, and it takes no values other than the session's own.
func TestConfigOfTheSessionWantsASession(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	config := filepath.Join(t.TempDir(), "dsvs.json")
	if err := os.WriteFile(config, []byte(`{"apps":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := printSession(&out, &shape{config: config}, nil, "", false); err == nil || !strings.Contains(err.Error(), build.Name()+" start opens one") {
		t.Errorf("--session without a session = %v, want a refusal that says what to do", err)
	}
	if err := printSession(&out, &shape{config: config}, []string{"api=demo"}, "", false); err == nil || !strings.Contains(err.Error(), build.Name()+" set changes them") {
		t.Errorf("--session with a value = %v, want a refusal", err)
	}
	if out.Len() > 0 {
		t.Errorf("a refusal wrote %q", out.String())
	}
}
