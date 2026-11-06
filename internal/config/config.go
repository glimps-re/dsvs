package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dsvs/internal/build"
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
var Names = namesFor(build.Name())

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

	// Observed is what the machine showed when this configuration was built.
	Observed *Observed `json:"-"`

	// Answer is the answer of the program it comes from, as written: a handover carries it, and the new
	// image rebuilds the configuration in force from it without evaluating anything (Answered).
	Answer json.RawMessage `json:"-"`
}

// Input is what an evaluation receives: the human's intentions, layer by layer, and what the machine
// shows. Asked is what a gesture asks for, Network what is kept for the observed network, Session what
// was chosen since the session started, Kept what is kept for the project. The cascade that derives a
// value from them is the engine's, because it depends on the rules: Go merges nothing.
type Input struct {
	Asked    map[string]string `json:"asked"`
	Network  map[string]string `json:"network"`
	Session  map[string]string `json:"session"`
	Kept     map[string]string `json:"kept"`
	Observed *Observed         `json:"observed"`
}

// Observed is what the machine shows and nobody sets. nil: nothing observes it.
type Observed struct {
	Links []Link `json:"links"`
	Exits []Exit `json:"exits"`
}

// String says the network in a word, for the log, the state and the interfaces: each interface once,
// the ones traffic exits through first with their gateway, then the other links. Without an exit it is
// offline, and the links still visible only qualify it: a bare eth0 used to read as a network.
func (o *Observed) String() string {
	if o == nil {
		return "not observed"
	}
	shown := make([]string, 0, len(o.Exits)+len(o.Links))
	named := map[string]bool{}
	for _, exit := range o.Exits {
		if named[exit.Dev] {
			continue
		}
		named[exit.Dev] = true
		shown = append(shown, exitLabel(exit))
	}
	for _, link := range o.Links {
		if !named[link.Name] {
			shown = append(shown, link.Name)
		}
	}
	switch {
	case len(o.Exits) > 0:
		return strings.Join(shown, ", ")
	case len(shown) > 0:
		return "offline (" + strings.Join(shown, ", ") + ")"
	}
	return "offline"
}

// Network is what names this network for the values kept on it: the preferred exit, as String writes
// it. Empty when nothing observes it or nothing exits — there is then no network to keep anything
// for.
func (o *Observed) Network() string {
	if o == nil || len(o.Exits) == 0 {
		return ""
	}
	return exitLabel(o.Exits[0])
}

// Exit is a default route: the gateway, empty for a route without one, and the interface.
type Exit struct {
	Via string `json:"via"`
	Dev string `json:"dev"`
}

type Link struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"`
	Addrs []string `json:"addrs"`
}

// Pairs writes values the way set takes them, name=value, sorted by name: that is how the log and the
// state quote them.
func Pairs(values map[string]string) string {
	shown := make([]string, 0, len(values))
	for _, name := range slices.Sorted(maps.Keys(values)) {
		shown = append(shown, name+"="+values[name])
	}
	return strings.Join(shown, " ")
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

// Release is what a stop does with the apps some names bring along: those it stops, and those it keeps
// because a facade that was not named still serves them.
type Release struct {
	Stops []string
	Keeps []Kept
}

// Kept is an app a stop keeps, and the facades that still serve it.
type Kept struct {
	App string
	By  []string
}

// Release splits wanted — the apps these names bring along, as the session resolves them — between
// what a stop takes down and what it keeps. Stopping a facade stops only the apps of its routes that no
// other facade serves: stopping one address does not cut the one next to it. A named app, or a member
// of a named group, stops regardless.
func (c Config) Release(names, wanted []string) Release {
	var facades, asked []string
	for _, name := range names {
		if _, app := c.App(name); !app && slices.ContainsFunc(c.Ingresses, func(in Ingress) bool { return in.Name == name }) {
			facades = append(facades, name)
			continue
		}
		apps, _ := c.AppsOf(name)
		asked = append(asked, apps...)
	}
	var release Release
	for _, app := range wanted {
		var named, others []string
		for _, in := range c.IngressesOf(app) {
			if slices.Contains(facades, in.Name) {
				named = append(named, in.Name)
			} else {
				others = append(others, in.Name)
			}
		}
		if len(named) > 0 && len(others) > 0 && !slices.Contains(asked, app) {
			release.Keeps = append(release.Keeps, Kept{App: app, By: others})
		} else {
			release.Stops = append(release.Stops, app)
		}
	}
	return release
}

// String says the stop in one line when it keeps something, and nothing otherwise: a stop that takes
// down everything named has nothing to explain.
func (r Release) String() string {
	if len(r.Keeps) == 0 {
		return ""
	}
	said := []string{"nothing to stop"}
	if len(r.Stops) > 0 {
		said[0] = "stopping " + strings.Join(r.Stops, ", ")
	}
	for _, kept := range r.Keeps {
		said = append(said, "keeping "+kept.App+", still served by "+strings.Join(kept.By, ", "))
	}
	return strings.Join(said, "; ")
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
	Ready   *Ready            `json:"ready,omitzero"`

	// Ansi is the resolved color, from 1 to 6; the interfaces consume it without going back to the name.
	Ansi int `json:"-"`
}

// Ready is what says an app is ready when its command does not give it away: a 2xx response on a
// path, or a line of its output. Only one of the two, and nothing else: no port, no check command.
// Without it, the command decides — an app a facade serves is ready when a real request gets an
// answer, an app that receives {port} without a route when its port opens, and an app that receives
// none as soon as it is launched.
type Ready struct {
	HTTP string   `json:"http,omitzero"`
	Log  *Pattern `json:"log,omitzero"`
}

// Pattern is what an output line must contain: a literal text, or a RegExp from the configuration,
// which the engine passes along as its source and flags, and which Go reads back as RE2.
type Pattern struct {
	Text   string
	RegExp string
	Flags  string
}

// patternJSON is the shape of a RegExp on the wire: JSON has none, the engine writes it this way.
type patternJSON struct {
	RegExp string `json:"regexp"`
	Flags  string `json:"flags,omitzero"`
}

func (p *Pattern) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &p.Text); err == nil {
		return nil
	}
	var re patternJSON
	if err := json.Unmarshal(data, &re); err != nil {
		return err
	}
	p.RegExp, p.Flags = re.RegExp, re.Flags
	return nil
}

func (p Pattern) MarshalJSON() ([]byte, error) {
	if p.RegExp == "" {
		return json.Marshal(p.Text)
	}
	return json.Marshal(patternJSON{RegExp: p.RegExp, Flags: p.Flags})
}

// Compile returns the expression a line must satisfy. The flags i, m and s keep their meaning, those
// that change nothing on a single line (g, y, d, u, v) stay silent; what RE2 cannot read — a
// lookbehind, a backreference — is an error, which validation refuses along with the configuration.
func (p Pattern) Compile() (*regexp.Regexp, error) {
	if p.RegExp == "" {
		if p.Text == "" {
			return nil, errors.New("the text is empty")
		}
		return regexp.Compile(regexp.QuoteMeta(p.Text))
	}
	modes := ""
	for _, flag := range p.Flags {
		switch flag {
		case 'i', 'm', 's':
			modes += string(flag)
		case 'g', 'y', 'd', 'u', 'v':
		default:
			return nil, fmt.Errorf("unknown flag %q", flag)
		}
	}
	if modes != "" {
		modes = "(?" + modes + ")"
	}
	return regexp.Compile(modes + p.RegExp)
}

// Resolved restores what JSON does not carry: the resolved color is derived again from the color's
// name, which does travel. A declaration that went through a handover passes here, otherwise an app
// that nothing declares any more, and that lives only on it, would lose its color.
func (s App) Resolved() App {
	s.Ansi = ansiByName[s.Color]
	return s
}

// SameRun says whether two declarations launch the same process. The color is not part of it: the
// interfaces read it from the current declaration, and an app need not restart when it changes. Nor is
// Ready: it only says what is expected of the process, and the next launch will take it.
func (s App) SameRun(other App) bool {
	s.Color, s.Ansi, s.Ready, other.Color, other.Ansi, other.Ready = "", 0, nil, "", 0, nil
	return reflect.DeepEqual(s, other)
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

// Listens says whether the app receives a port, in its command or its environment: without one,
// nothing reaches it, and nothing waits for it to listen.
func (s App) Listens() bool {
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

// colorOf derives from its name the color of an app that declares none: it stays the app's own when
// another app enters or leaves the configuration, and from one project to the next. Two apps may land
// on the same one; color tells them apart.
func colorOf(name string) string {
	hash := fnv.New32a()
	hash.Write([]byte(name))
	return palette[hash.Sum32()%uint32(len(palette))]
}

var ansiByName = map[string]int{"red": 1, "green": 2, "yellow": 3, "blue": 4, "magenta": 5, "cyan": 6}

// Load evaluates the configuration for this input: the form, always, and the configuration it builds
// when it is valid — nil otherwise, and the form says why. What comes out is validated: nothing
// downstream has anything left to check. A configuration is a program that receives the input and
// returns JSON — node is one implementation of it, a .json the degenerate case. watch are the command
// line's paths, validated along with the file's: a path that watches nothing must say so at load.
func Load(path string, watch []string, in Input) (Form, *Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Form{}, nil, err
	}
	out, echo, err := evaluate(abs, in)
	if err != nil {
		return Form{}, nil, err
	}
	return read(path, abs, watch, in, out, echo)
}

// Answered reads what an evaluation of this configuration answered, without redoing it, and validates
// it as if the answer had just arrived: that is how a handover takes over the configuration in force,
// whether or not the new binary knows how to build the file.
func Answered(path string, watch []string, in Input, answer []byte) (Form, *Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Form{}, nil, err
	}
	return read(path, abs, watch, in, answer, nil)
}

// read turns an answer into the form and the configuration it describes, validated.
func read(path, abs string, watch []string, in Input, out []byte, echo []string) (Form, *Config, error) {
	var answer struct {
		Form   Form `json:"form"`
		Config any  `json:"config"`
	}
	// A number stays written as is: that is what tells 4380 from 4380.5 where an integer is expected.
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.UseNumber()
	if err := decoder.Decode(&answer); err != nil {
		return Form{}, nil, fmt.Errorf("%s: unreadable output: %w", path, err)
	}
	if problems := answer.Form.validate(); len(problems) > 0 {
		return Form{}, nil, fault(path, problems)
	}
	if answer.Config == nil {
		if len(answer.Form.Problems()) == 0 {
			return Form{}, nil, fmt.Errorf("%s: the form is valid, and built no configuration", path)
		}
		return answer.Form, nil, nil
	}
	// The shape is checked before decoding, which keeps quiet about a key it does not know and reports
	// a value of the wrong type in Go's words.
	if problems := shaped("", answer.Config, reflect.TypeFor[Config]()); len(problems) > 0 {
		return Form{}, nil, fault(path, problems)
	}
	var built struct {
		Config *Config `json:"config"`
	}
	if err := json.Unmarshal(out, &built); err != nil {
		return Form{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg := built.Config
	cfg.Answer = out
	cfg.Echo = echo
	cfg.Observed = in.Observed
	cfg.ForcedWatch = watch
	cfg.resolve(abs)
	if err := cfg.validate(path); err != nil {
		return Form{}, nil, err
	}
	return answer.Form, cfg, nil
}

// evaluate runs the configuration and returns the JSON it writes along with what it said on its
// console. It is the one place where the extension decides who reads it: a .json is a bare
// configuration, without a form, a module goes through node which strips its types, any other file is
// a program, with the same contract.
func evaluate(abs string, in Input) ([]byte, []string, error) {
	if filepath.Ext(abs) == ".json" {
		out, err := os.ReadFile(abs)
		return append(append([]byte(`{"form":{"fields":[]},"config":`), out...), '}'), nil, err
	}
	answering, done := context.WithTimeout(context.Background(), evaluationLimit)
	defer done()
	program, err := programFor(answering, abs)
	if err != nil {
		return nil, nil, err
	}
	input, err := json.Marshal(in.normalized())
	if err != nil {
		return nil, nil, err
	}
	// A grandchild that keeps stdout open must not hold the read back once the program is killed.
	program.WaitDelay = time.Second
	program.Env = append(os.Environ(), "DSVS_CONFIG="+abs, "DSVS_INPUT="+string(input))
	program.Dir = filepath.Dir(abs)
	// A script that hangs must not outlive the tool: even outside the systemd box, the kernel takes it
	// down with us.
	program.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	// What the script says is captured, not relayed: during a hot reload, the binary's stderr is the
	// TUI's screen. A failure carries it along — that is where the real cause is.
	var console bytes.Buffer
	program.Stderr = &console
	out, err := program.Output()
	if answering.Err() != nil {
		return nil, nil, fmt.Errorf("%s: did not answer within %s%s", abs, evaluationLimit, block(console.String()))
	}
	if err != nil {
		// The reason reads from the project's folder: its absolute paths say nothing more there, and the
		// stack, in the log, keeps them.
		dir := filepath.Dir(abs) + string(filepath.Separator)
		why := strings.NewReplacer("file://"+dir, "", dir, "").Replace(thrown(console.String(), err))
		return nil, nil, fmt.Errorf("%s: %s%s", at(abs, console.String()), why, block(console.String()))
	}
	return out, lines(console.String()), nil
}

// at says where a program failed, file:line:column, like a compiler — that is what an editor knows
// how to open: the first call of its stack that is in a file, the configuration's or a file it
// imports, otherwise the header node puts on a syntax error, whose caret gives the column. The
// evaluated file, for lack of anything better.
func at(abs, console string) string {
	if missing := unfound.FindStringSubmatch(console); missing != nil {
		if line, column, found := imports(missing[2], missing[1]); found {
			return located(missing[2], strconv.Itoa(line), strconv.Itoa(column))
		}
	}
	for _, frame := range called.FindAllStringSubmatch(console, -1) {
		if !strings.Contains(frame[1], "[eval") {
			return located(frame[1], frame[2], frame[3])
		}
	}
	if header := pointed.FindStringSubmatch(console); header != nil {
		return located(header[1], header[2], strconv.Itoa(len(header[3])+1))
	}
	return abs
}

// imports says where this file imports this missing module: the line and column of the string that
// names it, as written — a package, or a path leading to it from the file's folder. node, which cannot
// find it, only gives the module and the file importing it.
func imports(importer, missing string) (line, column int, found bool) {
	source, err := os.ReadFile(importer)
	if err != nil {
		return 0, 0, false
	}
	for i, text := range strings.Split(string(source), "\n") {
		for _, quoted := range specifier.FindAllStringSubmatchIndex(text, -1) {
			named := text[quoted[2]:quoted[3]]
			if named == missing || strings.HasPrefix(named, missing+"/") || filepath.Join(filepath.Dir(importer), named) == missing {
				return i + 1, quoted[0] + 1, true
			}
		}
	}
	return 0, 0, false
}

// located writes a place that node names by a path or by a file URL.
func located(file, line, column string) string {
	if parsed, err := url.Parse(file); err == nil && parsed.Scheme == "file" {
		file = parsed.Path
	}
	return file + ":" + line + ":" + column
}

var (
	// unfound is the module an import cannot find, and the file importing it.
	unfound = regexp.MustCompile(`Cannot find (?:module|package) '([^']+)' imported from (\S+)`)
	// specifier is a quoted string, where an import names its module.
	specifier = regexp.MustCompile(`["']([^"'\n]+)["']`)
	// called is a stack call: "at f (file:///…/x.ts:12:5)", or without a function name.
	called = regexp.MustCompile(`(?m)^\s+at (?:.*\()?(file://[^\s()]+|/[^\s():]+):(\d+):(\d+)\)?$`)
	// pointed is the header of an error: the file and its line, the code leading to it — one line or
	// several — and the caret under the column.
	pointed = regexp.MustCompile(`(?m)^(file://\S+|/\S+):(\d+)\n(?:.*\n)*?( *)\^`)
)

// thrown names a program's failure by the error it threw — the line node writes at the top of its
// stack, TypeError: … — failing that by the last thing it said, and otherwise by its exit code: it is
// the first line of the error, the one an interface with room for a single line reads.
func thrown(console string, exit error) string {
	if line := raised.FindString(console); line != "" {
		return line
	}
	if said := lines(console); len(said) > 0 {
		return said[len(said)-1]
	}
	return exit.Error()
}

var raised = regexp.MustCompile(`(?m)^(?:[A-Z][A-Za-z]*)?Error\b[^:\n]*: .+$`)

// programFor returns the command that evaluates this file: node for a module, the file itself
// otherwise.
func programFor(ctx context.Context, abs string) (*exec.Cmd, error) {
	switch filepath.Ext(abs) {
	case ".ts", ".mts", ".js", ".mjs":
		return exec.CommandContext(ctx, "node", "--input-type=module", "-e", shim), nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("%s: neither a module node reads (.ts .mts .js .mjs) nor an executable program", abs)
	}
	return exec.CommandContext(ctx, abs), nil
}

// normalized returns the input with empty collections rather than null: that is what the program, in
// JavaScript or in shell, can walk without a guard.
func (in Input) normalized() Input {
	for _, layer := range []*map[string]string{&in.Asked, &in.Network, &in.Session, &in.Kept} {
		if *layer == nil {
			*layer = map[string]string{}
		}
	}
	if in.Observed != nil {
		seen := Observed{Links: slices.Clone(in.Observed.Links), Exits: slices.Clone(in.Observed.Exits)}
		if seen.Links == nil {
			seen.Links = []Link{}
		}
		if seen.Exits == nil {
			seen.Exits = []Exit{}
		}
		for i := range seen.Links {
			if seen.Links[i].Addrs == nil {
				seen.Links[i].Addrs = []string{}
			}
		}
		in.Observed = &seen
	}
	return in
}

// evaluationLimit bounds an evaluation: a script that never returns would otherwise hold the reload in
// progress forever. It is a variable so that a test can shorten it.
var evaluationLimit = 10 * time.Second

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
			return "", missing(from)
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

// shim is what node runs. console.log is sent to stderr so that stdout carries only the JSON. The hook
// serves "dsvs" to the configuration and to what it imports: the binary's engine answers, whether a
// types package is installed or not. An export made by config() resolves; without a form, the export is
// a configuration, or a function called without a value, and the form is empty.
var shim = `
console.log = console.error;
const { registerHooks } = await import("node:module");
const { pathToFileURL } = await import("node:url");
const engine = ` + quoted(engine) + `;
registerHooks?.({
  resolve: (specifier, context, next) => (specifier === "dsvs" ? { url: "dsvs:engine", shortCircuit: true } : next(specifier, context)),
  load: (url, context, next) => (url === "dsvs:engine" ? { format: "module", source: engine, shortCircuit: true } : next(url, context)),
});
const input = JSON.parse(process.env.DSVS_INPUT);
const mod = await import(pathToFileURL(process.env.DSVS_CONFIG).href).catch((error) => {
  if (!registerHooks && error?.code === "ERR_MODULE_NOT_FOUND") {
    error.message += "; node " + process.version + ' cannot serve "dsvs" to a configuration: form() needs node 22.15 or later';
  }
  throw error;
});
const resolve = mod.default?.[Symbol.for("dsvs.resolve")];
if (!resolve && "params" in mod) {
  process.stderr.write('export const params is replaced by form() from "dsvs": see the types\n');
  process.exit(1);
}
const answer = resolve
  ? await resolve(input)
  : { form: { fields: [] }, config: (typeof mod.default === "function" ? await mod.default({}, input.observed) : mod.default) ?? null };
// JSON has no RegExp: its source and flags go across, and Go reads it back.
process.stdout.write(JSON.stringify(answer, (_, value) => (value instanceof RegExp ? { regexp: value.source, flags: value.flags } : value)));
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
			c.Apps[i].Color = colorOf(c.Apps[i].Name)
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
		case !s.Listens() && len(c.IngressesOf(s.Name)) > 0:
			fail("%s: a facade routes to it, so the command must say where to listen, with %s", s.Name, TokenPort)
		}
		s.Ready.validate(s, fail)
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

	return fault(path, problems)
}

// fault returns a file's problems as an error, or nothing if it has none.
func fault(path string, problems []string) error {
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

// validate refuses what would never say an app is ready: an empty or double key, a path that does not
// start with /, a path on an app that receives no port, an expression Go cannot read.
func (r *Ready) validate(app App, fail func(string, ...any)) {
	switch {
	case r == nil:
	case (r.HTTP == "") == (r.Log == nil):
		fail("%s: ready takes http or log, and only one", app.Name)
	case r.Log != nil:
		if _, err := r.Log.Compile(); err != nil {
			fail("%s: ready.log: %v", app.Name, err)
		}
	case !strings.HasPrefix(r.HTTP, "/"):
		fail("%s: ready.http %q must start with /", app.Name, r.HTTP)
	case !app.Listens():
		fail("%s: ready.http asks %s, so the command must say where to listen, with %s", app.Name, r.HTTP, TokenPort)
	}
}

// validateTarget checks that a route leads to a single thing, and that this thing exists.
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

// exitLabel writes an exit: the interface, and the gateway when the route has one.
func exitLabel(exit Exit) string {
	switch {
	case exit.Via == "":
		return exit.Dev
	case exit.Dev == "":
		return "via " + exit.Via
	}
	return exit.Dev + " via " + exit.Via
}

// shaped says how a value does not have the shape t describes, in the configuration's words: a key t
// does not know, with the known key that resembles it, and a value of another type, each by its path.
// null counts as a missing key, as in decoding.
func shaped(at string, value any, t reflect.Type) []string {
	if value == nil {
		return nil
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if _, text := value.(string); t == reflect.TypeFor[Pattern]() {
		if text {
			return nil
		}
		t = reflect.TypeFor[patternJSON]()
	}
	switch t.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return []string{mistyped(at, "an object", value)}
		}
		keys := keysOf(t)
		var problems []string
		for _, key := range slices.Sorted(maps.Keys(object)) {
			i := slices.IndexFunc(keys, func(known reflect.StructField) bool { return known.Name == key })
			if i < 0 {
				problems = append(problems, unknown(at, key, keys))
				continue
			}
			problems = append(problems, shaped(within(at, key), object[key], keys[i].Type)...)
		}
		return problems
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return []string{mistyped(at, "an object", value)}
		}
		var problems []string
		for _, key := range slices.Sorted(maps.Keys(object)) {
			problems = append(problems, shaped(within(at, key), object[key], t.Elem())...)
		}
		return problems
	case reflect.Slice:
		list, ok := value.([]any)
		if !ok {
			return []string{mistyped(at, "a list", value)}
		}
		var problems []string
		for i, item := range list {
			problems = append(problems, shaped(fmt.Sprintf("%s[%d]", at, i), item, t.Elem())...)
		}
		return problems
	case reflect.String:
		if _, ok := value.(string); !ok {
			return []string{mistyped(at, "a text", value)}
		}
	case reflect.Bool:
		if _, ok := value.(bool); !ok {
			return []string{mistyped(at, "true or false", value)}
		}
	case reflect.Int:
		number, ok := value.(json.Number)
		if !ok {
			return []string{mistyped(at, "a number", value)}
		}
		if _, err := number.Int64(); err != nil {
			return []string{mistyped(at, "a whole number", value)}
		}
	}
	return nil
}

// keysOf returns the keys a configuration type reads, each with its type, in the order it declares
// them: Name there is the key as written.
func keysOf(t reflect.Type) []reflect.StructField {
	var keys []reflect.StructField
	for field := range t.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if field.IsExported() && name != "-" && name != "" {
			field.Name = name
			keys = append(keys, field)
		}
	}
	return keys
}

// unknown reports a key no field reads, and the known key it most likely meant: the closest one, when
// it is close enough to be just a typo.
func unknown(at, key string, keys []reflect.StructField) string {
	said := fmt.Sprintf("unknown key %q", key)
	closest, best := "", 0
	for _, known := range keys {
		if apart := distance(strings.ToLower(key), strings.ToLower(known.Name)); apart <= max(1, len(known.Name)/3) && (closest == "" || apart < best) {
			closest, best = known.Name, apart
		}
	}
	if closest != "" {
		said += fmt.Sprintf(" (did you mean %q?)", closest)
	}
	return prefixed(at, said)
}

// mistyped says what a value should have been, and what it is.
func mistyped(at, wanted string, value any) string {
	var is string
	switch value := value.(type) {
	case map[string]any:
		is = "an object"
	case []any:
		is = "a list"
	default:
		text, _ := json.Marshal(value)
		is = string(text)
	}
	return prefixed(at, wanted+", not "+is)
}

// within is the path of a key under another.
func within(at, key string) string {
	if at == "" {
		return key
	}
	return at + "." + key
}

func prefixed(at, said string) string {
	if at == "" {
		return said
	}
	return at + ": " + said
}

// distance counts the keystrokes between two words: a letter added, removed, changed, or two
// neighbors swapped.
func distance(a, b string) int {
	x, y := []rune(a), []rune(b)
	rows := make([][]int, len(x)+1)
	for i := range rows {
		rows[i] = make([]int, len(y)+1)
		rows[i][0] = i
	}
	for j := range rows[0] {
		rows[0][j] = j
	}
	for i := 1; i <= len(x); i++ {
		for j := 1; j <= len(y); j++ {
			changed := 1
			if x[i-1] == y[j-1] {
				changed = 0
			}
			rows[i][j] = min(rows[i-1][j]+1, rows[i][j-1]+1, rows[i-1][j-1]+changed)
			if i > 1 && j > 1 && x[i-1] == y[j-2] && x[i-2] == y[j-1] {
				rows[i][j] = min(rows[i][j], rows[i-2][j-2]+1)
			}
		}
	}
	return rows[len(x)][len(y)]
}

// namesFor gives the configurations a binary looks for. The beta has its own and never falls back to
// the stable's: a file written for the stable runs on the beta only once someone has decided so.
func namesFor(binary string) []string {
	if binary == build.Beta {
		return []string{"config.beta" + Suffix, "dsvs.beta.json"}
	}
	return []string{FileName, JSONName}
}

func missing(from string) error {
	err := fmt.Errorf("no %s found searching up from %s", strings.Join(Names, " or "), from)
	if Names[0] == FileName {
		return err
	}
	return fmt.Errorf("%w: the beta reads its own configuration, never the stable's — start from a copy of %s", err, FileName)
}
