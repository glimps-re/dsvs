package config

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dsvs/internal/build"
)

func TestLoadRunsAScript(t *testing.T) {
	t.Parallel()
	// The configuration is a script: what counts is the object it produces, not its shape.
	path := write(t, `
const app = (name) => ({ name, command: ["serve", name, "--port", "{port}"] });
export default async () => ({
    port: 4200,
    ingress: [{
        host: "localhost",
        routes: [
            { path: "/shop/", app: "shop" },
            { path: "/", redirect: "/shop/" },
        ],
    }],
    apps: ["shop", "admin"].map(app),
});`)

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Apps) != 2 || cfg.Apps[1].Name != "admin" {
		t.Fatalf("apps = %+v", cfg.Apps)
	}
	if cfg.Apps[0].Color != "cyan" || cfg.Apps[0].Ansi != 6 {
		t.Errorf("default color = %q (%d), want cyan (6)", cfg.Apps[0].Color, cfg.Apps[0].Ansi)
	}
	if cfg.Apps[0].Dir != filepath.Dir(path) || cfg.Dir != filepath.Dir(path) {
		t.Errorf("directory = %q / %q, want that of the configuration file", cfg.Apps[0].Dir, cfg.Dir)
	}
	// A facade is named by its host, and a baseHref is written as is: the trailing slash is dropped on
	// load so that the path serves as a prefix.
	in := cfg.Ingresses[0]
	if in.Name != "localhost" {
		t.Errorf("facade name = %q, want its host", in.Name)
	}
	if in.Routes[0].Path != "/shop" {
		t.Errorf("path = %q, want no trailing slash", in.Routes[0].Path)
	}
}

// The configuration is code, and code chatters: what it writes to the console is captured for the log of
// the session — written to the binary's stderr, it would draw over the TUI screen.
func TestLoadKeepsWhatTheScriptSays(t *testing.T) {
	t.Parallel()
	path := write(t, `
console.log("targets of", "shop");
export default {
    port: 4200,
    ingress: [{ host: "localhost", routes: [{ path: "/a", app: "a" }] }],
    apps: [{ name: "a", command: ["srv", "{port}"] }],
}`)

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Echo) != 1 || !strings.Contains(cfg.Echo[0], "targets of shop") {
		t.Errorf("Echo = %q, want what the script said", cfg.Echo)
	}
}

// A variable set to undefined is not set, as the types promise: apps with different variables, built
// from one form, fit in one array.
func TestAnUndefinedVariableIsNotSet(t *testing.T) {
	t.Parallel()
	path := write(t, `
const [tracing, delay] = [true, 200];
export default {
    port: 4200,
    ingress: [{ host: "localhost", routes: [{ path: "/", app: "front" }, { path: "/api", app: "api" }] }],
    apps: [
        { name: "front", command: ["srv", "{port}"], env: { TRACING: String(tracing), DELAY_MS: undefined } },
        { name: "api", command: ["srv", "{port}"], env: { DELAY_MS: String(delay) } },
    ],
}`)

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"TRACING": "true"}; !maps.Equal(cfg.Apps[0].Env, want) {
		t.Errorf("env of front = %v, want %v", cfg.Apps[0].Env, want)
	}
	if want := map[string]string{"DELAY_MS": "200"}; !maps.Equal(cfg.Apps[1].Env, want) {
		t.Errorf("env of api = %v, want %v", cfg.Apps[1].Env, want)
	}
}

// A script that crashes must say why and where: "exit status 1" alone tells nothing, the real cause is on
// the error output of node, and it is what the first line says — the one an interface with a single line
// shows —, behind the file, line and column an editor knows how to open.
func TestLoadErrorsCarryTheCause(t *testing.T) {
	t.Parallel()
	path := write(t, "const discount = 40;\nif (discount > 30)\n    throw new Error(\"the worktree has no proxies\");")
	first := func(err error) string {
		line, _, _ := strings.Cut(fmt.Sprint(err), "\n")
		return line
	}

	if _, err := Load(path, nil); first(err) != path+":3:11: Error: the worktree has no proxies" {
		t.Errorf("first line %q, want the thrown error and its place", first(err))
	}

	path = write(t, "export default {\n  port: 4200,, apps: [] };")
	if _, err := Load(path, nil); !strings.HasPrefix(first(err), path+":2:14: SyntaxError") {
		t.Errorf("a syntax error reads %q", first(err))
	}

	// An error thrown in a file the configuration imports is reported where it is thrown.
	path = write(t, `import { target } from "./targets.ts";
export default { port: 4200, apps: [], ingress: [{ host: "a.localhost", routes: [{ path: "/", proxy: target() }] }] };`)
	imported := filepath.Join(filepath.Dir(path), "targets.ts")
	if err := os.WriteFile(imported, []byte("export function target() {\n  throw new Error(\"no target\");\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, nil); first(err) != imported+":2:9: Error: no target" {
		t.Errorf("an error thrown in an import reads %q", first(err))
	}

	// A missing import is reported at the line of the import, which node does not give, and its paths
	// read from the project.
	path = write(t, "// the apps live elsewhere\nimport { apps } from \"./apps.dsvs.ts\";\nimport { z } from 'zod-absent';\nexport default { port: 4200, apps };")
	if _, err := Load(path, nil); first(err) != path+":2:22: Error [ERR_MODULE_NOT_FOUND]: Cannot find module 'apps.dsvs.ts' imported from config.dsvs.ts" {
		t.Errorf("a missing import reads %q", first(err))
	}
	path = write(t, "import { z } from 'zod-absent';\nexport default { port: 4200 };")
	if _, err := Load(path, nil); !strings.HasPrefix(first(err), path+":1:19: Error [ERR_MODULE_NOT_FOUND]: Cannot find package 'zod-absent' imported from ") {
		t.Errorf("a missing package reads %q", first(err))
	}
}

// The Host of the request chooses the facade; the same path does not mean the same thing there.
func TestHostChoosesTheFacade(t *testing.T) {
	t.Parallel()
	path := write(t, `export default {
    port: 4200,
    ingress: [
        { host: "admin.localhost", routes: [{ path: "/admin", app: "admin" }] },
        { name: "fallback", routes: [{ path: "/", app: "shop" }] },
    ],
    apps: [
        { name: "shop", command: ["srv", "{port}"] },
        { name: "admin", command: ["srv", "{port}"] },
    ],
}`)

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"admin.localhost":      "admin.localhost",
		"admin.localhost:4200": "admin.localhost", // the browser always adds the port
		"ADMIN.localhost":      "admin.localhost", // a host is case-insensitive
		"localhost:4200":       "fallback",        // the facade without a host takes the rest
		"anything.else":        "fallback",
	}
	for host, want := range cases {
		in, known := cfg.Ingress(host)
		if !known || in.Name != want {
			t.Errorf("Ingress(%q) = %q (%v), want %q", host, in.Name, known, want)
		}
	}
	if got := cfg.IngressesOf("admin"); len(got) != 1 || got[0].Name != "admin.localhost" {
		t.Errorf("admin should be served only by its facade, got %+v", got)
	}
}

// A name starts apps: its own, those of a facade, those of a group. A facade never appears in the result,
// it is not a process.
func TestNamesResolveToApps(t *testing.T) {
	t.Parallel()
	path := write(t, `export default {
    port: 4200,
    ingress: [
        { name: "beta", host: "beta.localhost", routes: [
            { path: "/", app: "shop" },
            { path: "/api", proxy: { target: "https://api.example.com" } },
        ] },
        { name: "storefront", host: "storefront.localhost", routes: [
            { path: "/", proxy: { target: "https://api.example.com" } },
        ] },
    ],
    apps: [
        { name: "shop", command: ["srv", "{port}"] },
        { name: "admin", command: ["srv", "{port}"] },
    ],
    groups: [{ name: "everything", members: ["shop", "admin"] }],
}`)

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"shop":       {"shop"},
		"beta":       {"shop"},
		"storefront": {}, // a pure proxy facade starts nothing, and is not unknown for all that
		"everything": {"shop", "admin"},
	}
	for name, want := range cases {
		got, known := cfg.AppsOf(name)
		if !known || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("AppsOf(%q) = %v (%v), want %v", name, got, known, want)
		}
	}
	if _, known := cfg.AppsOf("nope"); known {
		t.Error("an unknown name should say it is unknown")
	}
}

func TestLoadRefusesWhatWouldBreakLater(t *testing.T) {
	t.Parallel()
	app := `{ name: "a", command: ["srv", "{port}"] }`
	front := `{ host: "localhost", routes: [{ path: "/a", app: "a" }] }`
	cases := map[string]string{
		"two apps with the same name": `{ port: 1, ingress: [` + front + `], apps: [
			` + app + `, ` + app + `] }`,
		"an app that does not say where to listen": `{ port: 1, ingress: [` + front + `],
			apps: [{ name: "a", command: ["srv"] }] }`,
		"no port": `{ ingress: [` + front + `], apps: [` + app + `] }`,
		"two facades on the same host": `{ port: 1, ingress: [` + front + `,
			{ name: "other", host: "localhost", routes: [{ path: "/a", app: "a" }] }],
			apps: [` + app + `] }`,
		"two facades with the same name": `{ port: 1, ingress: [` + front + `, ` + front + `],
			apps: [` + app + `] }`,
		"a facade bears the name of an app": `{ port: 1, ingress: [
			{ name: "a", host: "x", routes: [{ path: "/a", app: "a" }] }], apps: [` + app + `] }`,
		"route to an unknown app": `{ port: 1, ingress: [
			{ host: "x", routes: [{ path: "/a", app: "nope" }] }], apps: [` + app + `] }`,
		"route leading nowhere": `{ port: 1, ingress: [{ host: "x", routes: [{ path: "/a" }] }],
			apps: [` + app + `] }`,
		"route leading to two things": `{ port: 1, ingress: [{ host: "x",
			routes: [{ path: "/a", app: "a", redirect: "/b" }] }], apps: [` + app + `] }`,
		"two routes for the same path": `{ port: 1, ingress: [{ host: "x", routes: [
			{ path: "/a", app: "a" }, { path: "/a/", app: "a" }] }], apps: [` + app + `] }`,
		"redirect without a leading slash": `{ port: 1, ingress: [{ host: "x",
			routes: [{ path: "/a", redirect: "elsewhere" }] }], apps: [` + app + `] }`,
		"proxy target without a scheme": `{ port: 1, ingress: [{ host: "x",
			routes: [{ path: "/a", proxy: { target: "api.example.com" } }] }], apps: [` + app + `] }`,
		"proxy target without a host": `{ port: 1, ingress: [{ host: "x",
			routes: [{ path: "/a", proxy: { target: "https://" } }] }], apps: [` + app + `] }`,
		"facade on the host reserved for the catch-all": `{ port: 1, ingress: [{ host: "*",
			routes: [{ path: "/a", app: "a" }] }], apps: [` + app + `] }`,
		"facade without a route": `{ port: 1, ingress: [{ host: "x", routes: [] }], apps: [` + app + `] }`,
		"no facade":              `{ port: 1, ingress: [], apps: [` + app + `] }`,
		"empty command":          `{ port: 1, ingress: [` + front + `], apps: [{ name: "a", command: [] }] }`,
		"route without a leading slash": `{ port: 1, ingress: [{ host: "x",
			routes: [{ path: "a", app: "a" }] }], apps: [` + app + `] }`,
		"unknown color": `{ port: 1, ingress: [` + front + `],
			apps: [{ name: "a", color: "chartreuse", command: ["srv", "{port}"] }] }`,
		"group without a member": `{ port: 1, ingress: [` + front + `], apps: [` + app + `],
			groups: [{ name: "g", members: [] }] }`,
		"group bearing the name of an app": `{ port: 1, ingress: [` + front + `],
			apps: [` + app + `], groups: [{ name: "a", members: ["a"] }] }`,
		"two groups with the same name": `{ port: 1, ingress: [` + front + `], apps: [` + app + `],
			groups: [{ name: "g", members: ["a"] }, { name: "g", members: ["a"] }] }`,
		"unknown member": `{ port: 1, ingress: [` + front + `], apps: [` + app + `],
			groups: [{ name: "g", members: ["nope"] }] }`,
		"group in a group": `{ port: 1, ingress: [` + front + `], apps: [` + app + `],
			groups: [{ name: "g", members: ["a"] }, { name: "h", members: ["g"] }] }`,
		// A facade does not start: it is served as long as the session lives, it is not the stuff a group
		// is made of.
		"facade in a group": `{ port: 1, ingress: [` + front + `], apps: [` + app + `],
			groups: [{ name: "g", members: ["localhost"] }] }`,
		"member declared twice": `{ port: 1, ingress: [` + front + `], apps: [` + app + `],
			groups: [{ name: "g", members: ["a", "a"] }] }`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Load(write(t, "export default "+body), nil); err == nil {
				t.Fatal("configuration accepted although it is invalid")
			}
		})
	}
}

// A misspelled key does not silently change what runs, and a value of the wrong type is reported in the
// words of the configuration: both by their path, at every level.
func TestAKeyOrATypeTheConfigurationDoesNotReadIsRefusedInItsWords(t *testing.T) {
	t.Parallel()
	shop := `ingress: [{ host: "shop.localhost", routes: [{ path: "/", app: "front" }] }]`
	front := `{ name: "front", command: ["srv", "{port}"] }`
	cases := map[string]struct{ body, said string }{
		"misspelled key": {
			`{ port: 4380, ` + shop + `, apps: [` + front + `, { name: "api", command: ["api"], evn: { DISCOUNT: "20" } }] }`,
			`config.dsvs.ts: apps[1]: unknown key "evn" (did you mean "env"?)`,
		},
		"port as text": {
			`{ port: "4380", ` + shop + `, apps: [` + front + `] }`,
			`config.dsvs.ts: port: a number, not "4380"`,
		},
		"key without a close match, at the root": {
			`{ port: 4380, ` + shop + `, apps: [` + front + `], verbose: true }`,
			`config.dsvs.ts: unknown key "verbose"`,
		},
		"deep in a route": {
			`{ port: 4380, ingress: [{ host: "shop.localhost", routes: [{ path: "/", proxy: { target: "http://x", secure: "no", changorigin: true } }] }], apps: [` + front + `] }`,
			"config.dsvs.ts:\n  - ingress[0].routes[0].proxy: unknown key \"changorigin\" (did you mean \"changeOrigin\"?)\n  - ingress[0].routes[0].proxy.secure: true or false, not \"no\"",
		},
		"a whole number expected": {
			`{ port: 4380.5, ` + shop + `, apps: [` + front + `] }`,
			`config.dsvs.ts: port: a whole number, not 4380.5`,
		},
		"a list, a text": {
			`{ port: 4380, ` + shop + `, apps: [{ name: "front", command: "srv {port}", env: { DISCOUNT: 20 } }] }`,
			"config.dsvs.ts:\n  - apps[0].command: a list, not \"srv {port}\"\n  - apps[0].env.DISCOUNT: a text, not 20",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(write(t, "export default "+c.body), nil)
			if err == nil || !strings.HasSuffix(err.Error(), string(filepath.Separator)+c.said) {
				t.Errorf("refusal = %v, want %q", err, c.said)
			}
		})
	}
}

// The most specific path wins, whatever the order of writing, and "/" takes the rest.
func TestMatchByPath(t *testing.T) {
	t.Parallel()
	in := NewIngress(Ingress{Routes: []Route{
		{Path: "/shop-beta", App: "shop"},
		{Path: "/api/shop", Proxy: Target{URL: "https://api.example.com"}},
		{Path: "/admin", App: "admin"},
		{Path: "/admin/docs", App: "docs"}, // more specific than /admin, and declared after
		{Path: "/", Redirect: "/shop-beta/"},
	}})

	cases := map[string]string{
		"/admin/docs":          "docs",
		"/admin/docs/intro":    "docs",
		"/shop-beta":           "shop",
		"/shop-beta/main.js":   "shop",
		"/api/shop/orders":     "proxy",
		"/admin":               "admin",
		"/admin/":              "admin",
		"/api/v1/users?page=1": "redirect", // no route: the catch-all
		"/favicon.ico":         "redirect",
		"/adminis":             "redirect", // a prefix stops at a slash
	}
	for path, want := range cases {
		route, matched := in.Match(path)
		if !matched {
			t.Errorf("Match(%q) found nothing", path)
			continue
		}
		got := route.App
		switch route.Kind() {
		case ToProxy:
			got = "proxy"
		case ToRedirect:
			got = "redirect"
		}
		if got != want {
			t.Errorf("Match(%q) → %q, want %q", path, got, want)
		}
	}

	// Without a catch-all, a path without a route is not routed: it is a 404, not a default app.
	bare := Ingress{Routes: []Route{{Path: "/admin", App: "admin"}}}
	if _, matched := bare.Match("/favicon.ico"); matched {
		t.Error("a path without a route should lead nowhere")
	}
}

// A target is written like an entry of proxy.conf: its defaults are the same.
func TestTargetDefaults(t *testing.T) {
	t.Parallel()
	no := false
	cases := []struct {
		target Target
		verify bool
		host   string
	}{
		{Target{URL: "https://api.example.com"}, true, "api.example.com"},
		{Target{URL: "https://staging.example.com/api", Secure: &no}, false, "staging.example.com"},
		{Target{URL: "http://127.0.0.1:8090"}, true, "127.0.0.1:8090"},
	}
	for _, c := range cases {
		if got := c.target.Verify(); got != c.verify {
			t.Errorf("%s: certificate verified = %v, want %v", c.target, got, c.verify)
		}
		if got := c.target.Host(); got != c.host {
			t.Errorf("%s: host = %q, want %q", c.target, got, c.host)
		}
	}
}

func TestFindWalksUp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, FileName)
	if err := os.WriteFile(want, []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Find(deep)
	if err != nil || got != want {
		t.Fatalf("Find(%q) = %q, %v", deep, got, err)
	}
	if _, err := Find(filepath.Dir(os.TempDir())); err == nil || !strings.Contains(err.Error(), FileName) {
		t.Errorf("without a file, the error should name %s, got %v", FileName, err)
	}
}

// A static configuration is read as is: it is the same contract as the one the shim produces, and an app
// that is not a web application then has no reason to require node.
func TestLoadReadsAStaticConfiguration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, JSONName)
	body := `{
        "port": 4200,
        "ingress": [{ "host": "localhost", "routes": [{ "path": "/shop/", "app": "shop" }] }],
        "apps": [{ "name": "shop", "command": ["serve", "--port", "{port}"] }]
    }`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 4200 || len(cfg.Apps) != 1 || cfg.Apps[0].Dir != dir {
		t.Errorf("configuration = %+v, want the port, the app and its resolved directory", cfg)
	}
	if cfg.Subnet != defaultSubnet {
		t.Errorf("subnet = %q, the defaults also apply to the static one", cfg.Subnet)
	}
	if len(cfg.Echo) != 0 {
		t.Errorf("echo = %v, a static file says nothing", cfg.Echo)
	}
}

// A directory holding both wrote the static one for someone else: the script wins.
func TestFindPrefersTheScript(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range Names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := Find(dir); err != nil || got != filepath.Join(dir, FileName) {
		t.Errorf("Find = %q, %v; want %s", got, err, FileName)
	}
}

// The beta finds its own file and never the stable's: a directory holding only config.dsvs.ts gets a
// refusal that says how to start, and one holding both gives the beta its own.
func TestTheBetaReadsItsOwnFile(t *testing.T) {
	stable := Names
	Names = namesFor(build.Beta)
	defer func() { Names = stable }()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Find(dir)
	if err == nil || !strings.Contains(err.Error(), "config.beta.dsvs.ts") || !strings.Contains(err.Error(), "copy of "+FileName) {
		t.Fatalf("Find = %v; wanted a refusal naming the beta's file and how to start", err)
	}

	own := filepath.Join(dir, "config.beta.dsvs.ts")
	if err := os.WriteFile(own, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := Find(dir); err != nil || got != own {
		t.Errorf("Find = %q, %v; wanted %s", got, err, own)
	}
}

// The watched paths are those of the configuration and those of the session, relative to the file, and
// failing that its directory — the one most of them come from.
func TestWatchedPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	neighbor := filepath.Join(dir, "elsewhere.dsvs.ts")
	if err := os.WriteFile(neighbor, []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(`export default { port: 4200, watch: ["elsewhere.dsvs.ts"],
        ingress: [{ host: "h", routes: [{ path: "/", app: "s" }] }],
        apps: [{ name: "s", command: ["s", "--port", "{port}"] }] }`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The configuration is always watched, whatever its name: without it, saving it would reload
	// nothing.
	if got := cfg.Watched(); len(got) != 2 || got[0] != path || got[1] != neighbor {
		t.Errorf("watched = %v, want the file and the declared path, resolved as absolute", got)
	}

	cfg.ForcedWatch = []string{"/tmp"}
	if got := cfg.Watched(); len(got) != 3 || got[2] != "/tmp" {
		t.Errorf("watched = %v, the command line adds to the file", got)
	}

	cfg.Watch, cfg.ForcedWatch = nil, nil
	if got := cfg.Watched(); len(got) != 2 || got[1] != dir {
		t.Errorf("watched = %v, with nothing declared it is the directory of the configuration", got)
	}
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Launch replaces the tokens wherever they are written, in the command as in the environment, and leaves
// the app intact: the next run may land elsewhere.
func TestLaunchPlacesTheAddress(t *testing.T) {
	t.Parallel()
	app := App{
		Name:    "shop",
		Command: []string{"serve", "--host", "{ip}", "--port", "{port}"},
		Env:     map[string]string{"URL": "http://{ip}:{port}/", "FORCE_COLOR": "1"},
	}

	command, env := app.Launch("10.77.0.2", 41507)
	if want := "serve --host 10.77.0.2 --port 41507"; strings.Join(command, " ") != want {
		t.Errorf("command = %q, want %q", strings.Join(command, " "), want)
	}
	if got := env["URL"]; got != "http://10.77.0.2:41507/" {
		t.Errorf("URL = %q", got)
	}
	if got := env["FORCE_COLOR"]; got != "1" {
		t.Errorf("a value without a token should pass as is, got %q", got)
	}
	if app.Command[2] != "{ip}" || app.Env["URL"] != "http://{ip}:{port}/" {
		t.Error("Launch modified the app instead of returning a copy")
	}

	// The same app elsewhere: nothing sticks from the previous run.
	again, _ := app.Launch("127.0.0.1", 1234)
	if want := "serve --host 127.0.0.1 --port 1234"; strings.Join(again, " ") != want {
		t.Errorf("second run = %q, want %q", strings.Join(again, " "), want)
	}
}

func TestAConfigurationThatHangsIsCutShort(t *testing.T) {
	was := evaluationLimit
	evaluationLimit = 300 * time.Millisecond
	t.Cleanup(func() { evaluationLimit = was })
	// Without a timer, node exits by itself on an await that never resolves: the timer keeps the process alive.
	path := write(t, "setInterval(() => {}, 1000);\nawait new Promise(() => {});\nexport default {};")

	start := time.Now()
	_, err := Load(path, nil)
	if err == nil || !strings.Contains(err.Error(), "did not answer within") {
		t.Fatalf("Load = %v, want did not answer within", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the error came after %s, want under a second", took)
	}
}
