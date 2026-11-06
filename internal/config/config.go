package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Suffix marks every configuration file of this tool — the entry one as well as those it imports,
// starting with the targets nobody commits. It is this suffix that tells apart, in a watched folder,
// the files a configuration can be made of.
const (
	Suffix   = ".dsvs.ts"
	FileName = "config" + Suffix
	// JSONName is the configuration without node: an app that is not a web application has no reason
	// to require node.
	JSONName = "dsvs.json"
)

// Names are the configurations looked for while walking up the tree, in order: TypeScript first,
// because a repository that has both wrote the static one for someone else.
var Names = []string{FileName, JSONName}

type Config struct {
	Port      int       `json:"port"`
	Ingresses []Ingress `json:"ingress"`
	Apps      []App     `json:"apps"`
	Groups    []Group   `json:"groups,omitzero"`

	// Subnet is the range where the apps' virtual addresses live. It is configurable so as not to
	// collide with a VPN.
	Subnet string `json:"subnet,omitzero"`

	// Watch says what to watch besides the file itself: a configuration knows what it imports, and some
	// of its files live outside its folder — in the original one, when a worktree does not have its
	// own. A folder watches its configuration files, a file watches itself.
	Watch []string `json:"watch,omitzero"`

	// Path is the file all of this comes from, and Dir its folder: relative paths refer to it, and it is
	// through Path that a session rereads its configuration without having to remember an argument.
	Path string `json:"-"`
	Dir  string `json:"-"`

	// ForcedPort is the port imposed by the command line: it overrides the file's on every read,
	// reloads included — a session choice, which the file cannot take back.
	ForcedPort int `json:"-"`

	// ForcedWatch are the command line's paths, added to the file's: through them one watches a
	// configuration whose content one does not control, produced by a command or written by hand.
	ForcedWatch []string `json:"-"`

	// Echo is what the script wrote to its console during the evaluation. It goes to the session's log:
	// written on the binary's stderr, it would draw over the TUI's screen.
	Echo []string `json:"-"`
}

const defaultSubnet = "10.77.0.0/24"

// Watched returns the paths whose saving must make the configuration reread: the file itself, always —
// its name need not carry the suffix to be followed — those it declares, those of the command line,
// and failing those its folder, where most of its imports come from.
func (c Config) Watched() []string {
	declared := append(append([]string{}, c.Watch...), c.ForcedWatch...)
	if len(declared) == 0 {
		declared = []string{c.Dir}
	}
	return append([]string{c.Path}, declared...)
}

// Prefix returns the range of virtual addresses. Loading has validated it.
func (c Config) Prefix() netip.Prefix {
	prefix, _ := netip.ParsePrefix(c.Subnet)
	return prefix
}

// Group names a set that is started and read in a single gesture. Its members are apps: a facade has
// no place in it since it does not start, it is served as long as the session lives.
type Group struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

func (c Config) Group(name string) (Group, bool) {
	for _, g := range c.Groups {
		if g.Name == name {
			return g, true
		}
	}
	return Group{}, false
}

// Wanted resolves command-line names into apps to start: each name is an app, a facade — which brings
// along the apps of its routes — or a group, and gives the apps it brings along. An unknown name is an
// error, not a silence.
func (c Config) Wanted(args []string) ([]string, error) {
	var names []string
	for _, arg := range args {
		apps, known := c.AppsOf(arg)
		if !known {
			return nil, fmt.Errorf("no app, facade or group named %q", arg)
		}
		for _, name := range apps {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names, nil
}

// AppsOf returns the apps this name starts: an app names itself, a facade the apps of its routes, a
// group its members. A facade never appears in the result because it is not a process — it is a
// routing table on the session's port, always served. The second return says whether the name exists:
// a facade that only proxies starts nothing and is not unknown for all that.
func (c Config) AppsOf(name string) ([]string, bool) {
	if _, ok := c.App(name); ok {
		return []string{name}, true
	}
	for _, in := range c.Ingresses {
		if in.Name == name {
			return in.Apps(), true
		}
	}
	if group, known := c.Group(name); known {
		return group.Members, true
	}
	return nil, false
}

// Ingress is a facade: a host, and the table that says what becomes of each path. It decides the
// routing; an app does not know the paths through which it is reached.
type Ingress struct {
	Name   string  `json:"name,omitzero"`
	Host   string  `json:"host,omitzero"`
	Routes []Route `json:"routes"`
}

// Route says what becomes of a request whose path falls under Path: it goes to an app, to a proxy,
// or bounces back as a redirect. Exactly one of the three, loading checks it.
type Route struct {
	Path     string `json:"path"`
	App      string `json:"app,omitzero"`
	Proxy    Target `json:"proxy,omitzero"`
	Redirect string `json:"redirect,omitzero"`
}

// Target is a proxy target, written like a proxy.conf.json entry: same words, same defaults. A route
// has only one: the configuration is the only place where the backend changes, and hot reload rereads
// it without restarting anything.
type Target struct {
	URL          string `json:"target"`
	Secure       *bool  `json:"secure,omitzero"`
	ChangeOrigin bool   `json:"changeOrigin,omitzero"`
}

// Verify says whether to verify the target's certificate.
func (t Target) Verify() bool { return t.Secure == nil || *t.Secure }

// Host is the target's host, as it appears in the logs and, when needed, in the request.
func (t Target) Host() string {
	if parsed, err := url.Parse(t.URL); err == nil {
		return parsed.Host
	}
	return t.URL
}

func (t Target) String() string { return t.URL }

// Kind names what a route does, for the interfaces.
type Kind int

const (
	ToApp Kind = iota
	ToProxy
	ToRedirect
)

func (r Route) Kind() Kind {
	switch {
	case r.Proxy.URL != "":
		return ToProxy
	case r.Redirect != "":
		return ToRedirect
	}
	return ToApp
}

// Covers says whether the path falls under this route. "/" takes everything left: it is the
// catch-all, and without it a path with no route is a 404.
func (r Route) Covers(path string) bool {
	if r.Path == "/" {
		return true
	}
	return path == r.Path || strings.HasPrefix(path, r.Path+"/")
}

// Match returns the route that takes this path — an already decoded path, without query, as
// r.URL.Path gives it. Routes are sorted from most specific to most general at load: the first one
// that covers is the right one.
func (i Ingress) Match(path string) (Route, bool) {
	for _, route := range i.Routes {
		if route.Covers(path) {
			return route, true
		}
	}
	return Route{}, false
}

// Serves says whether this facade leads to this app.
func (i Ingress) Serves(app string) bool {
	for _, route := range i.Routes {
		if route.App == app {
			return true
		}
	}
	return false
}

// PathsOf returns the paths of this facade that lead to the app.
func (i Ingress) PathsOf(app string) []string {
	var out []string
	for _, route := range i.Routes {
		if route.App == app {
			out = append(out, route.Path)
		}
	}
	return out
}

// Apps returns the apps this facade serves, in the order of its routes.
func (i Ingress) Apps() []string {
	var out []string
	for _, route := range i.Routes {
		if route.App != "" && !slices.Contains(out, route.App) {
			out = append(out, route.App)
		}
	}
	return out
}

// App is a command to run. It does not choose its address: the network gives it one at start and
// puts it in the command where the tokens mark it.
type App struct {
	Name    string            `json:"name"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitzero"`
	Dir     string            `json:"dir,omitzero"`
	Color   string            `json:"color,omitzero"`

	// Ansi is the resolved color, from 1 to 6; the interfaces consume it without going back to the name.
	Ansi int `json:"-"`
}

// Tokens replaced in the command and the environment at start: through them an app learns where to
// listen, without anyone having a port to write.
const (
	TokenIP   = "{ip}"
	TokenPort = "{port}"
)

// Launch returns the command and the environment of a run for the given address. A pure function: it
// does not touch the app, which may be restarted elsewhere.
func (s App) Launch(ip string, port int) ([]string, map[string]string) {
	replace := strings.NewReplacer(TokenIP, ip, TokenPort, strconv.Itoa(port)).Replace
	command := make([]string, 0, len(s.Command))
	for _, arg := range s.Command {
		command = append(command, replace(arg))
	}
	env := make(map[string]string, len(s.Env))
	for k, v := range s.Env {
		env[k] = replace(v)
	}
	return command, env
}

// saysWhereToListen says whether the app has a place for its port: without one, the tool would not
// know where to reach it.
func (s App) saysWhereToListen() bool {
	for _, arg := range s.Command {
		if strings.Contains(arg, TokenPort) {
			return true
		}
	}
	for _, v := range s.Env {
		if strings.Contains(v, TokenPort) {
			return true
		}
	}
	return false
}

var palette = []string{"cyan", "magenta", "green", "yellow", "blue", "red"}

var ansiByName = map[string]int{"red": 1, "green": 2, "yellow": 3, "blue": 4, "magenta": 5, "cyan": 6}

// Load reads the configuration file and returns a validated object: nothing downstream has anything
// left to check. A .json is read as is, everything else is a module that node evaluates — it is the
// same contract, and the shim only translates it. watch are the command line's paths, validated along
// with the file's: a path that watches nothing must say so at load.
func Load(path string, watch []string) (Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	var (
		out  []byte
		echo []string
	)
	if filepath.Ext(abs) == ".json" {
		out, err = os.ReadFile(abs)
	} else {
		out, echo, err = evaluate(abs)
	}
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(out, &cfg); err != nil {
		return Config{}, fmt.Errorf("%s: unreadable output: %w", path, err)
	}
	cfg.Echo = echo
	cfg.ForcedWatch = watch
	cfg.resolve(abs)
	return cfg, cfg.validate(path)
}

// evaluate has node evaluate a module, which strips its types, and returns the JSON it gets from it
// along with what the script said on its console.
func evaluate(abs string) ([]byte, []string, error) {
	node := exec.Command("node", "--input-type=module", "-e", shim)
	node.Env = append(os.Environ(), "DSVS_CONFIG="+abs)
	node.Dir = filepath.Dir(abs)
	// What the script says is captured, not relayed: during a hot reload, the binary's stderr is the
	// TUI's screen. A failure carries it along — that is where the real cause is.
	var console bytes.Buffer
	node.Stderr = &console
	out, err := node.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w%s", abs, err, block(console.String()))
	}
	return out, lines(console.String()), nil
}

// Find walks up the tree from dir looking for a configuration.
func Find(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// from is the folder the search starts from: that is the one to name when nothing is found, not the
	// root where the walk stops.
	from := dir
	for {
		for _, name := range Names {
			candidate := filepath.Join(dir, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found searching up from %s", strings.Join(Names, " or "), from)
		}
		dir = parent
	}
}

func (c Config) App(name string) (App, bool) {
	for _, s := range c.Apps {
		if s.Name == name {
			return s, true
		}
	}
	return App{}, false
}

// IngressesOf returns the facades that serve this app.
func (c Config) IngressesOf(app string) []Ingress {
	var out []Ingress
	for _, in := range c.Ingresses {
		if in.Serves(app) {
			out = append(out, in)
		}
	}
	return out
}

// Ingress returns the facade that answers for this host: the one declaring it, or failing that the one
// declaring none. The header's port does not count, the browser always puts it there.
func (c Config) Ingress(host string) (Ingress, bool) {
	host = strings.ToLower(host)
	if bare, _, err := net.SplitHostPort(host); err == nil {
		host = bare
	}
	var open *Ingress
	for i := range c.Ingresses {
		switch in := &c.Ingresses[i]; {
		case in.Host == host:
			return *in, true
		case in.Host == "" && open == nil:
			open = in
		}
	}
	if open == nil {
		return Ingress{}, false
	}
	return *open, true
}

// console.log is sent to stderr so that stdout carries only the JSON.
const shim = `
console.log = console.error;
const { pathToFileURL } = await import("node:url");
const mod = await import(pathToFileURL(process.env.DSVS_CONFIG).href);
const value = typeof mod.default === "function" ? await mod.default() : mod.default;
process.stdout.write(JSON.stringify(value ?? null));
`

func (c *Config) resolve(path string) {
	root := filepath.Dir(path)
	c.Path, c.Dir = path, root
	for i := range c.Apps {
		if c.Apps[i].Dir == "" {
			c.Apps[i].Dir = root
		} else if !filepath.IsAbs(c.Apps[i].Dir) {
			c.Apps[i].Dir = filepath.Join(root, c.Apps[i].Dir)
		}
		if c.Apps[i].Color == "" {
			c.Apps[i].Color = palette[i%len(palette)]
		}
		c.Apps[i].Ansi = ansiByName[c.Apps[i].Color]
	}
	if c.Subnet == "" {
		c.Subnet = defaultSubnet
	}
	for i, path := range c.Watch {
		if !filepath.IsAbs(path) {
			c.Watch[i] = filepath.Join(root, path)
		}
	}
	for i := range c.Ingresses {
		c.Ingresses[i] = NewIngress(c.Ingresses[i])
	}
}

// NewIngress returns a facade ready to route: its name, its host and its paths normalized, and its
// routes sorted from most specific to most general — that is the order they are tried in, and it does
// not depend on the declaration's. Loading goes through here, and so does a facade built by hand.
func NewIngress(in Ingress) Ingress {
	in.Host = strings.ToLower(in.Host)
	// A facade is named after its host: that is its identity, and what one wants to read.
	if in.Name == "" {
		in.Name = in.Host
		if in.Name == "" {
			in.Name = "ingress"
		}
	}
	for r := range in.Routes {
		// A route path carries no trailing slash, unlike a baseHref: both can therefore be written the
		// same way in the configuration.
		if route := &in.Routes[r]; len(route.Path) > 1 {
			route.Path = strings.TrimSuffix(route.Path, "/")
		}
	}
	slices.SortStableFunc(in.Routes, func(a, b Route) int { return len(b.Path) - len(a.Path) })
	return in
}

func (c Config) validate(path string) error {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if len(c.Apps) == 0 {
		fail("no app declared")
	}
	if len(c.Ingresses) == 0 {
		fail("no facade declared")
	}
	if c.Port <= 0 {
		fail("missing port: it is the only open port, and it is declared at the top of the configuration")
	}
	if prefix, err := netip.ParsePrefix(c.Subnet); err != nil {
		fail("unreadable subnet %q: %v", c.Subnet, err)
	} else if size := prefix.Addr().BitLen() - prefix.Bits(); 1<<size < len(c.Apps)+3 {
		fail("subnet %s too small for %d apps", c.Subnet, len(c.Apps))
	}

	for _, path := range c.Watched() {
		if _, err := os.Stat(path); err != nil {
			fail("watched path %s does not exist", path)
		}
	}

	names := map[string]bool{}
	for _, s := range c.Apps {
		switch {
		case s.Name == "":
			fail("an app has no name")
		case names[s.Name]:
			fail("two apps are named %q", s.Name)
		}
		names[s.Name] = true
		switch {
		case len(s.Command) == 0:
			fail("%s: command is empty", s.Name)
		case !s.saysWhereToListen():
			fail("%s: the command must say where to listen, with %s", s.Name, TokenPort)
		}
		if _, known := ansiByName[s.Color]; !known {
			fail("%s: unknown color %q (%s)", s.Name, s.Color, strings.Join(palette, ", "))
		}
	}

	seen, hosts := map[string]bool{}, map[string]string{}
	for _, in := range c.Ingresses {
		switch {
		case names[in.Name]:
			fail("facade %q has the name of an app", in.Name)
		case seen[in.Name]:
			fail("two facades are named %q", in.Name)
		}
		seen[in.Name] = true
		// The hub keeps this name to mean "everything nobody else claims": a facade writing it would be a
		// catch-all on its side and not on ours.
		if in.Host == "*" {
			fail("%s: host \"*\" is reserved; an empty host takes what nobody claims", in.Name)
		}
		if owner, taken := hosts[in.Host]; taken {
			fail("%s and %s claim the same host", owner, in.Name)
		}
		hosts[in.Host] = in.Name

		if len(in.Routes) == 0 {
			fail("%s: no route declared", in.Name)
		}
		paths := map[string]bool{}
		for _, route := range in.Routes {
			if !strings.HasPrefix(route.Path, "/") {
				fail("%s: route %q must start with /", in.Name, route.Path)
			}
			if paths[route.Path] {
				fail("%s: two routes for %q", in.Name, route.Path)
			}
			paths[route.Path] = true
			in.validateTarget(route, names, fail)
		}
	}

	groups := map[string]bool{}
	for _, g := range c.Groups {
		switch {
		case g.Name == "":
			fail("a group has no name")
		case names[g.Name] || seen[g.Name]:
			fail("group %q has the name of an app or a facade", g.Name)
		case groups[g.Name]:
			fail("two groups are named %q", g.Name)
		}
		groups[g.Name] = true
		if len(g.Members) == 0 {
			fail("%s: no member declared", g.Name)
		}
		members := map[string]bool{}
		for _, member := range g.Members {
			switch {
			case !names[member]:
				// A group holds only apps: no cycle to fear, no deep resolution to write, and what it
				// starts is exactly what it names.
				fail("%s: member %q is not an app", g.Name, member)
			case members[member]:
				fail("%s: member %q is declared twice", g.Name, member)
			}
			members[member] = true
		}
	}

	switch len(problems) {
	case 0:
		return nil
	case 1:
		// A single problem goes on the file's line: a one-item bullet list only adds a line break, and a
		// one-line error is what a terminal shows best.
		return fmt.Errorf("%s: %s", path, problems[0])
	}
	return fmt.Errorf("%s:\n  - %s", path, strings.Join(problems, "\n  - "))
}

// lines splits an output into lines, without the blank ones.
func lines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// block puts an output under the error it explains, or nothing if there is nothing to say.
func block(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "\n" + strings.TrimRight(text, "\n")
}

// validateTarget checks that a route leads to exactly one thing, and that this thing exists.
func (i Ingress) validateTarget(route Route, apps map[string]bool, fail func(string, ...any)) {
	declared := 0
	for _, on := range []bool{route.App != "", route.Proxy.URL != "", route.Redirect != ""} {
		if on {
			declared++
		}
	}
	if declared != 1 {
		fail("%s: route %q must lead to an app, a proxy or a redirect, and to only one",
			i.Name, route.Path)
		return
	}
	switch route.Kind() {
	case ToApp:
		if !apps[route.App] {
			fail("%s: route %q leads to app %q, which does not exist", i.Name, route.Path, route.App)
		}
	case ToRedirect:
		if !strings.HasPrefix(route.Redirect, "/") {
			fail("%s: the redirect of %q must start with /", i.Name, route.Path)
		}
	case ToProxy:
		parsed, err := url.Parse(route.Proxy.URL)
		switch {
		case err != nil:
			fail("%s: route %q has an unreadable target (%q): %v", i.Name, route.Path, route.Proxy.URL, err)
		case parsed.Scheme != "http" && parsed.Scheme != "https":
			fail("%s: target %q of %q must be http or https", i.Name, route.Proxy.URL, route.Path)
		case parsed.Host == "":
			fail("%s: target %q of %q has no host", i.Name, route.Proxy.URL, route.Path)
		}
	}
}
