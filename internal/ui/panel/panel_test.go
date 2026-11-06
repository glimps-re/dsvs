package panel

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"dsvs/internal/attach"
	"dsvs/internal/config"
	"dsvs/internal/projects"
)

// What is seen and what can be clicked must coincide: one button per thing the app allows, and never
// two zones on top of each other — otherwise a click would do something other than what it shows.
func TestZonesMatchWhatIsOffered(t *testing.T) {
	shown, with := demo(t)
	_, zones, _ := paint(shown, looking, with, time.Now(), aside)

	offered := map[string][]deed{}
	for _, z := range zones {
		offered[z.name] = append(offered[z.name], z.do)
	}
	if got := offered["front"]; len(got) != 2 || !has(got, stop) || !has(got, restart) {
		t.Errorf("a ready app offers %v, wanted stop and restart", got)
	}
	if got := offered["admin"]; len(got) != 1 || !has(got, start) {
		t.Errorf("a stopped app offers %v, wanted start", got)
	}
	// Two running sessions, a cold project: twice "stop apps" and the TUI, one start, one forget per
	// project, the two global gestures, and the single cross.
	if got := offered[""]; count(got, closeAll) != 2 || count(got, openTUI) != 2 || count(got, open) != 1 ||
		count(got, forget) != 3 || count(got, stopEverything) != 1 || count(got, quitDsvs) != 1 ||
		count(got, dismiss) != 1 {
		t.Errorf("the actions without an app are %v", got)
	}
	canvas, _, _ := paint(shown, looking, with, time.Now(), aside)
	checkZones(t, canvas, zones)
}

// checkZones requires every zone to be clickable — 24 × 24 at least — to fit in the canvas and to
// overlap no other.
func checkZones(t *testing.T, canvas *image.RGBA, zones []zone) {
	t.Helper()
	for i, a := range zones {
		if a.at.Dx() < 24 || a.at.Dy() < 24 {
			t.Errorf("zone %v of %v is under 24 × 24", a.at, a.do)
		}
		if !a.at.In(canvas.Bounds()) {
			t.Errorf("zone %v overflows the canvas %v", a.at, canvas.Bounds())
		}
		for _, b := range zones[i+1:] {
			if a.at.Intersect(b.at) != (image.Rectangle{}) {
				t.Errorf("two zones overlap: %v and %v", a.at, b.at)
			}
		}
	}
}

// The update offers only one button at a time, and none while it works: a gesture that can be pressed
// twice would start two updates.
func TestTheUpdateOffersOneButtonAtATime(t *testing.T) {
	shown, with := demo(t)
	cases := []struct {
		news   renewal
		wanted deed
	}{
		{renewal{stage: idle, said: "up to date"}, look},
		{renewal{stage: found, to: "v1.4.0"}, renew},
		{doing("building"), nothing},
	}
	for _, c := range cases {
		_, zones, _ := paint(shown, c.news, with, time.Now(), aside)
		var offered []deed
		for _, z := range zones {
			if z.do == look || z.do == renew {
				offered = append(offered, z.do)
			}
		}
		if c.wanted == nothing && len(offered) > 0 {
			t.Errorf("%+v offers %v, wanted nothing to click", c.news, offered)
		}
		if c.wanted != nothing && (len(offered) != 1 || offered[0] != c.wanted) {
			t.Errorf("%+v offers %v, wanted %v alone", c.news, offered, c.wanted)
		}
	}
}

// The beta's gesture stands on its own row, beside nothing it could be mistaken for, and leaves while
// something works: an install pressed twice would run twice.
func TestTheBetaIsOfferedOutsideOfWork(t *testing.T) {
	shown, with := demo(t)
	cases := []struct {
		news   renewal
		wanted deed
	}{
		{renewal{stage: idle, said: "up to date", beta: installBeta, betaOut: "v0.6.0-beta.1"}, installBeta},
		{renewal{stage: found, to: "v0.6.0-beta.2", beta: removeBeta}, removeBeta},
		{renewal{stage: idle, said: "up to date"}, nothing},
		{doing("downloading v0.6.0-beta.1"), nothing},
	}
	for _, c := range cases {
		canvas, zones, _ := paint(shown, c.news, with, time.Now(), aside)
		checkZones(t, canvas, zones)
		var offered []deed
		for _, z := range zones {
			if z.do == installBeta || z.do == removeBeta {
				offered = append(offered, z.do)
			}
		}
		if c.wanted == nothing && len(offered) > 0 || c.wanted != nothing && !slices.Equal(offered, []deed{c.wanted}) {
			t.Errorf("%+v offers %v, wanted %q alone", c.news, offered, c.wanted)
		}
	}
}

// A project whose session is coming up offers nothing to click: forgetting it removed its row, and
// the link that arrived next had nowhere left to show — the session ran, invisible.
func TestAProjectWakingUpOffersNothing(t *testing.T) {
	_, with := demo(t)
	shown := []project{{config: "/one/dsvs.json", waking: true}}
	_, zones, _ := paint(shown, looking, with, time.Now(), aside)

	for _, z := range zones {
		if z.config != "" {
			t.Errorf("a project waking up offers %v", z.do)
		}
	}
}

// An app that appears makes the panel taller: the content decides the window.
func TestThePanelGrowsWithItsContent(t *testing.T) {
	shown, with := demo(t)
	short, _, _ := paint(shown, looking, with, time.Now(), aside)
	shown[1].live.Apps = append(shown[1].live.Apps, attach.App{Name: "second", Status: "idle"})
	tall, _, _ := paint(shown, looking, with, time.Now(), aside)

	if tall.Bounds().Dy() <= short.Bounds().Dy() {
		t.Errorf("height %d with one more app, %d without", tall.Bounds().Dy(), short.Bounds().Dy())
	}
}

// Two worktrees of the same project have the same configuration folder: the shown name must go up one
// level to tell them apart, and no more.
func TestLabelsSayWhatSeparatesSessions(t *testing.T) {
	alone := labels([]project{{config: "/home/user/front/config.dsvs.ts"}})
	if alone["/home/user/front/config.dsvs.ts"] != "front" {
		t.Errorf("alone, a session is called %q", alone["/home/user/front/config.dsvs.ts"])
	}

	twins := labels([]project{
		{config: "/home/user/main/front/config.dsvs.ts"},
		{config: "/home/user/wt/front/config.dsvs.ts"},
	})
	one, two := twins["/home/user/main/front/config.dsvs.ts"], twins["/home/user/wt/front/config.dsvs.ts"]
	if one != "main/front" || two != "wt/front" {
		t.Errorf("the twins are called %q and %q", one, two)
	}
}

// The window opens, at the size of its content and where it was asked for.
func TestTheWindowOpensWhereItIsAsked(t *testing.T) {
	p := onDisplay(t)

	seen, err := xproto.GetWindowAttributes(p.conn, p.win).Reply()
	if err != nil {
		t.Fatal(err)
	}
	if seen.MapState != xproto.MapStateViewable {
		t.Errorf("the window is %v, wanted mapped", seen.MapState)
	}
	where, err := xproto.GetGeometry(p.conn, xproto.Drawable(p.win)).Reply()
	if err != nil {
		t.Fatal(err)
	}
	if int(where.Width) != p.size.X || int(where.Height) != p.size.Y {
		t.Errorf("window %dx%d, panel %v", where.Width, where.Height, p.size)
	}
	// The icon is near the top of the screen: the status bar is therefore at the top, and the panel
	// sits below it, aligned on its left edge.
	if int(where.X) != anchor.Min.X || int(where.Y) != anchor.Max.Y+gap {
		t.Errorf("window at %d,%d, wanted under the icon %v", where.X, where.Y, anchor)
	}
}

// demo is a full panel: two sessions whose apps cover every state, and a project already seen that is
// asleep.
func demo(t *testing.T) ([]project, faces) {
	t.Helper()
	with, err := load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sessions := []attach.State{
		{
			Config: "/home/user/Documents/shop/config.dsvs.ts", Port: 4200,
			Apps: []attach.App{
				{Name: "front", Status: "ready", Ready: true, PID: 4242, Since: now.Add(-14 * time.Minute)},
				{Name: "admin", Status: "idle", Last: "exited (2)", Fell: true},
				{Name: "storybook", Status: "starting", Since: now.Add(-2 * time.Second)},
				{Name: "docs", Status: "ready", Ready: true, Since: now.Add(-3 * time.Hour), Drift: "restart pending"},
			},
			Facades: []attach.Facade{{Host: "localhost"}, {Host: "admin.localhost"},
				{Host: "docs.localhost"}, {Host: "storybook.shop.localhost"}},
			Interfaces: []attach.Interface{{Kind: "tui", Window: "20971523"}},
		},
		{
			Config: "/home/user/wt/feat-cart/shop/config.dsvs.ts", Port: 4210,
			Apps:    []attach.App{{Name: "beta", Status: "idle"}},
			Facades: []attach.Facade{{Host: "beta.localhost"}},
		},
	}
	shown := []project{
		{config: sessions[0].Config, live: &sessions[0]},
		{config: sessions[1].Config, live: &sessions[1]},
		{config: "/home/user/Documents/shop-api/dsvs.json"},
	}
	return shown, with
}

// parametrized is the panel of a shop's front: two flags, the server its API comes from — a short
// select — the catalog the local API serves — a select of five values — and the network the machine
// observes, the office's.
func parametrized(t *testing.T) ([]project, faces) {
	t.Helper()
	with, err := load()
	if err != nil {
		t.Fatal(err)
	}
	state := attach.State{
		Config: "/one/dsvs.json", Port: 4300,
		Apps: []attach.App{{Name: "front", Status: "ready", Ready: true}},
		Params: []config.Field{
			{Name: "slowApi", Type: "flag", Choices: []string{"false", "true"}, Value: "true"},
			{Name: "verbose proxy", Type: "flag", Choices: []string{"false", "true"}, Value: "false"},
			{Name: "api", Type: "choice", Choices: []string{"local", "staging", "demo"}, Value: "local"},
			{Name: "catalog", Type: "choice", Choices: []string{"empty", "small", "full", "black-friday-2026", "broken-images"},
				Value: "black-friday-2026"},
		},
		Observed: &config.Observed{Links: []config.Link{{Name: "tun0", Type: "tun"}, {Name: "wlan0", Type: "wlan"}},
			Exits: []config.Exit{{Via: "198.51.100.2", Dev: "wlan0"}}},
	}
	return []project{{config: state.Config, live: &state}}, with
}

// A hidden field is not drawn, a disabled field is inert, whatever its type, and an imposed value
// reads without offering anything to click: only what a gesture can set has a zone.
func TestOnlyWhatAGestureCanSetOffersAZone(t *testing.T) {
	shown, with := parametrized(t)
	shown[0].live = withState(shown[0].live, func(s *attach.State) { s.Params = inert })
	canvas, zones, _ := paint(shown, looking, with, time.Now(), aside)
	checkZones(t, canvas, zones)
	named := map[string]bool{}
	for _, z := range zones {
		if z.do == choose || z.do == drop || z.do == edit {
			named[z.name] = true
		}
	}
	if want := map[string]bool{"api": true, "products": true, "branch": true}; !reflect.DeepEqual(named, want) {
		t.Errorf("parameter zones %v, wanted %v", named, want)
	}
}

// inert is a form carrying each state of a field: three are set by a gesture, a choice, a number and
// a text.
var inert = []config.Field{
	{Name: "api", Type: "choice", Choices: []string{"local", "staging"}, Value: "staging"},
	{Name: "deployed.version", Type: "choice", Choices: []string{"v2", "v3"}, Value: "v2", Hidden: true},
	{Name: "deployed.traces", Type: "flag", Choices: []string{"false", "true"}, Value: "false", Disabled: true,
		Reasons: []string{"traces are only reachable from the company network"}},
	{Name: "deployed.retries", Type: "number", Value: "3", Disabled: true},
	{Name: "deployed.token", Type: "text", Value: "t0k3n", Disabled: true, Reasons: []string{"set by the vault"}},
	{Name: "locale", Type: "choice", Choices: []string{"en", "fr"}, Value: "en", Readonly: true},
	{Name: "products", Type: "number", Value: "250", Min: bound(0), Max: bound(10000)},
	{Name: "branch", Type: "text", Value: "main"},
	{Name: "slowApi", Type: "flag", Choices: []string{"false", "true"}, Value: "false", Hidden: true},
}

func bound(value float64) *float64 { return &value }

// A group is written under its title, and leaves with it when everything it contains is hidden.
func TestAGroupIsTitledAndLeavesWithItsFields(t *testing.T) {
	shown, with := parametrized(t)
	height := func(fields ...config.Field) int {
		shown[0].live = withState(shown[0].live, func(s *attach.State) { s.Params = fields })
		canvas, zones, _ := paint(shown, looking, with, time.Now(), aside)
		checkZones(t, canvas, zones)
		return canvas.Bounds().Dy()
	}
	api := config.Field{Name: "api", Type: "choice", Choices: []string{"local", "staging"}, Value: "local"}
	version := config.Field{Name: "deployed.version", Type: "choice", Choices: []string{"v2", "v3"}, Value: "v2"}
	traces := config.Field{Name: "deployed.traces", Type: "flag", Choices: []string{"false", "true"}, Value: "false"}
	alone := height(api)
	if got := height(api, version, traces); got != alone+titleHeight+2*rowHeight {
		t.Errorf("a group of two fields takes %d px, wanted its title and two rows (%d)", got-alone, titleHeight+2*rowHeight)
	}
	version.Hidden, traces.Hidden = true, true
	if got := height(api, version, traces); got != alone {
		t.Errorf("a fully hidden group leaves %d px", got-alone)
	}
}

// A disabled field's reasons are written under it, with nothing of an error, like a kept value that
// yields, in amber; a field error is written under the field, a form error under the block, in the
// error color.
func TestReasonsAndErrorsAreWrittenUnderWhatTheyConcern(t *testing.T) {
	shown, with := parametrized(t)
	drawn := func(change func(*attach.State)) *image.RGBA {
		one := append([]project(nil), shown...)
		one[0].live = withState(shown[0].live, change)
		canvas, zones, _ := paint(one, looking, with, time.Now(), aside)
		checkZones(t, canvas, zones)
		return canvas
	}
	products := config.Field{Name: "products", Type: "number", Value: "250"}
	calm := drawn(func(s *attach.State) { s.Params = []config.Field{products} })
	if slices.Contains(colorsOf(calm), brokenText) {
		t.Fatal("a valid form is written in the error color")
	}

	disabled := products
	disabled.Disabled, disabled.Reasons = true, []string{"only the local api has a catalog"}
	reasoned := drawn(func(s *attach.State) { s.Params = []config.Field{disabled} })
	if got := reasoned.Bounds().Dy() - calm.Bounds().Dy(); got != errorRow+6 || slices.Contains(colorsOf(reasoned), brokenText) {
		t.Errorf("a disabled field's reason takes %d px, wanted one line (%d) with nothing of an error", got, errorRow+6)
	}

	yielding := products
	yielding.Yielded = &config.Yield{Value: "12000", From: "kept", Errors: []string{"at most 10000"}}
	yielded := drawn(func(s *attach.State) { s.Params = []config.Field{yielding} })
	if got := yielded.Bounds().Dy() - calm.Bounds().Dy(); got < errorRow+6 || !slices.Contains(colorsOf(yielded), waking) || slices.Contains(colorsOf(yielded), brokenText) {
		t.Errorf("a kept value that yields takes %d px, wanted its lines (%d each) in amber, with nothing of an error", got, errorRow)
	}

	noticed := drawn(func(s *attach.State) {
		s.Params, s.Notices = []config.Field{products}, []string{"remise is hidden, yet apps.api.env.REMISE depends on it"}
	})
	if noticed.Bounds().Dy() <= calm.Bounds().Dy() || !slices.Contains(colorsOf(noticed), waking) || slices.Contains(colorsOf(noticed), brokenText) {
		t.Error("a form notice is not written under the block, in amber, with nothing of an error")
	}

	wrong := products
	wrong.Errors = []string{"at most 10000"}
	for what, change := range map[string]func(*attach.State){
		"field": func(s *attach.State) { s.Params = []config.Field{wrong} },
		"form":  func(s *attach.State) { s.Params, s.Errors = []config.Field{products}, []string{"demo has no fr build"} },
	} {
		if canvas := drawn(change); canvas.Bounds().Dy() != calm.Bounds().Dy()+errorRow+6 || !slices.Contains(colorsOf(canvas), brokenText) {
			t.Errorf("a %s error is not written on its line, in the error color", what)
		}
	}
}

// A disabled option shows, greyed out, its reason under the field, and offers nothing to click: as
// segments as in an unfolded list.
func TestADisabledChoiceIsShownAndOffersNothing(t *testing.T) {
	shown, with := parametrized(t)
	api := config.Field{Name: "api", Type: "choice", Choices: []string{"local", "staging", "demo"}, Value: "local",
		DisabledChoices: map[string]string{"staging": "staging needs the VPN"}}
	long := api
	long.Choices = []string{"local", "staging", "demo", "a-server-whose-name-does-not-fit-in-segments"}
	for _, field := range []config.Field{api, long} {
		one := append([]project(nil), shown...)
		one[0].live = withState(shown[0].live, func(s *attach.State) { s.Params = []config.Field{field} })
		seen := aside
		seen.dropped = slot{one[0].config, "api"}
		_, zones, _ := paint(one, looking, with, time.Now(), seen)
		var offered []string
		for _, z := range zones {
			if z.do == choose && z.name == "api" {
				offered = append(offered, z.value)
			}
		}
		if slices.Contains(offered, "staging") || !slices.Contains(offered, "demo") {
			t.Errorf("%d choices: the panel offers %q, wanted everything but staging", len(field.Choices), offered)
		}
	}
}

// azerty is a reduced keyboard table, as an X server returns it: the second column is Shift, the
// fifth AltGr; a letter only has its lower case.
var azerty = func() keyboard {
	rows := map[xproto.Keycode][]xproto.Keysym{
		9: {xkEscape}, 10: {'&', '1'}, 12: {'"', '3'}, 17: {'_', '8', 0, 0, '\\'}, 19: {0xe0, '0'},
		22: {xkBackSpace}, 24: {'a'}, 36: {xkReturn}, 87: {xkKPEnd, 0xffb1}, 110: {xkHome}, 113: {xkLeft},
		119: {xkDelete},
	}
	k := keyboard{first: 8, per: 6, syms: make([]xproto.Keysym, 6*248)}
	for code, row := range rows {
		copy(k.syms[(int(code)-8)*6:], row)
	}
	return k
}()

// A key writes what the current layout makes it write: Shift, Caps Lock for a letter, AltGr and Num
// Lock decide.
func TestAKeyWritesWhatTheLayoutSays(t *testing.T) {
	const shift, lock, numLock, altGr = xproto.ModMaskShift, xproto.ModMaskLock, xproto.ModMask2, xproto.ModMask5
	for _, c := range []struct {
		code  xproto.Keycode
		state uint16
		want  rune
	}{
		{10, 0, '&'}, {10, shift, '1'}, {10, lock, '&'},
		{24, 0, 'a'}, {24, shift, 'A'}, {24, lock, 'A'}, {24, shift | lock, 'a'},
		{17, altGr, '\\'}, {87, numLock, '1'}, {87, 0, 0}, {200, 0, 0},
	} {
		if got := runeOf(azerty.symbol(c.code, c.state)); got != c.want {
			t.Errorf("keycode %d under %#x writes %q, wanted %q", c.code, c.state, got, c.want)
		}
	}
}

// A click on a text or a number opens it for entry, on its value. What is typed is written at the
// cursor, Enter sends the value to the session, and Escape or a click elsewhere abandon it without
// sending anything; outside an entry, Escape closes the panel.
func TestTypingSendsOnEnterAndNothingOtherwise(t *testing.T) {
	shown, with := parametrized(t)
	const one = "/one/dsvs.json"
	var said []map[string]string
	p := &panel{order: []string{one}, state: map[string]attach.State{one: *withState(shown[0].live, func(s *attach.State) { s.Params = inert })},
		replaying: map[string]change{}, refused: map[string]string{}, replayed: make(chan replay, 4),
		done: make(chan struct{}), failed: map[string]string{}, waking: map[string]bool{},
		pending: map[string]map[string]time.Time{}, keys: azerty}
	p.setting = func(_ string, values map[string]string) error {
		said = append(said, values)
		return nil
	}
	held, given := 0, 0
	p.claim = func() (func(), bool) {
		held++
		return func() { given++ }, true
	}
	editing := func(name string) {
		t.Helper()
		canvas, zones, _ := paint(p.snapshot(), looking, with, time.Now(), p.view())
		checkZones(t, canvas, zones)
		for _, z := range zones {
			if z.do == edit && z.name == name {
				p.act(z)
				return
			}
		}
		t.Fatalf("%s offers no entry", name)
	}
	keys := func(strokes ...[2]int) {
		for _, stroke := range strokes {
			if !p.pressed(xproto.KeyPressEvent{Detail: xproto.Keycode(stroke[0]), State: uint16(stroke[1])}) {
				t.Fatal("a key of the entry closed the panel")
			}
		}
	}
	backspace, enter, cancel, home, del := [2]int{22, 0}, [2]int{36, 0}, [2]int{9, 0}, [2]int{110, 0}, [2]int{119, 0}
	three, zero := [2]int{12, xproto.ModMaskShift}, [2]int{19, xproto.ModMaskShift}

	editing("products")
	if got := string(p.typing.text); got != "250" || p.typing.at != 3 {
		t.Fatalf("the entry opens on %q, cursor at %d, wanted 250 and the cursor at the end", got, p.typing.at)
	}
	canvas, zones, _ := paint(p.snapshot(), looking, with, time.Now(), p.view())
	checkZones(t, canvas, zones)
	for _, z := range zones {
		if z.name == "products" {
			t.Errorf("the entry in progress offers a zone %v", z.do)
		}
	}
	keys(backspace, backspace, backspace, three, zero, zero, enter)
	<-p.replayed
	delete(p.replaying, one)
	if want := []map[string]string{{"products": "300"}}; !reflect.DeepEqual(said, want) {
		t.Errorf("the entry asked for %v, wanted %v", said, want)
	}
	if p.typing.name != "" {
		t.Error("the entry stays open after Enter")
	}

	editing("branch")
	keys(home, del, [2]int{24, xproto.ModMaskShift}, [2]int{10, 0})
	if got := string(p.typing.text); got != "A&ain" {
		t.Errorf("the entry at the start gives %q, wanted A&ain", got)
	}
	keys(cancel)
	editing("branch")
	keys([2]int{24, 0})
	p.act(zone{do: drop, config: one, name: "api"})
	if len(said) != 1 || p.typing.name != "" {
		t.Errorf("Escape or a click elsewhere sent %v, or left the entry open", said[1:])
	}
	if p.pressed(xproto.KeyPressEvent{Detail: escape}) {
		t.Error("outside an entry, Escape does not close the panel")
	}
	if held != 3 || given != 3 {
		t.Errorf("three entries took the focus %d times and gave it back %d times, wanted 3 and 3", held, given)
	}
}

// An entry only exists with the focus: refused, the box does not switch to entry and says how to set
// the field, until the next gesture; lost along the way, the entry ends without taking it back from
// whoever took it, and keys write nothing anymore.
func TestTypingNeedsTheKeyboard(t *testing.T) {
	shown, with := parametrized(t)
	const one = "/one/dsvs.json"
	p := &panel{order: []string{one}, state: map[string]attach.State{one: *withState(shown[0].live, func(s *attach.State) { s.Params = inert })},
		replaying: map[string]change{}, refused: map[string]string{}, replayed: make(chan replay, 4),
		done: make(chan struct{}), failed: map[string]string{}, waking: map[string]bool{},
		pending: map[string]map[string]time.Time{}, keys: azerty}
	p.setting = func(_ string, values map[string]string) error {
		t.Errorf("without the keyboard, the panel asked for %v", values)
		return nil
	}
	drawn := func() (*image.RGBA, zone) {
		canvas, zones, _ := paint(p.snapshot(), looking, with, time.Now(), p.view())
		checkZones(t, canvas, zones)
		for _, z := range zones {
			if z.do == edit && z.name == "products" {
				return canvas, z
			}
		}
		t.Fatal("products offers no entry")
		return nil, zone{}
	}

	p.claim = func() (func(), bool) { return nil, false }
	calm, z := drawn()
	p.act(z)
	told, _ := drawn()
	if p.typing.name != "" || told.Bounds().Dy() <= calm.Bounds().Dy() {
		t.Errorf("without the keyboard, the entry opens (%q) or says nothing (%d more px)", p.typing.name, told.Bounds().Dy()-calm.Bounds().Dy())
	}
	if !p.pressed(xproto.KeyPressEvent{Detail: 12, State: xproto.ModMaskShift}) || p.typing.name != "" {
		t.Error("without an entry, a key writes somewhere")
	}
	p.act(zone{do: drop, config: one, name: "api"})
	if again, _ := drawn(); again.Bounds().Dy() != calm.Bounds().Dy() {
		t.Error("the note about the keyboard survives the next gesture")
	}

	given := 0
	p.claim = func() (func(), bool) { return func() { given++ }, true }
	_, z = drawn()
	p.act(z)
	p.lost()
	p.pressed(xproto.KeyPressEvent{Detail: 36})
	if p.typing.name != "" || given != 0 {
		t.Errorf("the lost focus leaves the entry open (%q), or the panel takes it back (%d)", p.typing.name, given)
	}
}

// A flag is a switch whose whole row is clickable, and a click sends the opposite of its value; what
// the machine observes is shown without offering anything to click.
func TestAFlagToggleSendsTheOppositeValue(t *testing.T) {
	shown, with := parametrized(t)
	canvas, zones, _ := paint(shown, looking, with, time.Now(), aside)
	checkZones(t, canvas, zones)

	var chosen []zone
	for _, z := range zones {
		if z.do == choose && (z.name == "slowApi" || z.name == "verbose proxy") {
			chosen = append(chosen, z)
		}
	}
	if len(chosen) != 2 {
		t.Fatalf("the flags' zones are %v, wanted one per flag", chosen)
	}
	for _, z := range zones {
		if z.name == "network" {
			t.Errorf("what is observed offers %v", z)
		}
	}

	var said []map[string]string
	p := &panel{replaying: map[string]change{}, refused: map[string]string{}, replayed: make(chan replay, 2),
		done: make(chan struct{})}
	p.setting = func(config string, values map[string]string) error {
		if config != "/one/dsvs.json" {
			t.Errorf("the change goes to %s", config)
		}
		said = append(said, values)
		return nil
	}
	for _, z := range chosen {
		p.act(z)
		<-p.replayed
		delete(p.replaying, z.config)
	}
	want := []map[string]string{{"slowApi": "false"}, {"verbose proxy": "true"}}
	if !reflect.DeepEqual(said, want) {
		t.Errorf("the panel asked for %v, wanted %v", said, want)
	}
}

// A select with short values is a frame of segments: a click on a segment sends its value — not the
// next one — and the chosen value is not a zone. A select of more than four values fits in a button
// whose list unfolds in the panel, one at a time; choosing a value folds it.
func TestSelectsChooseBySegmentOrByList(t *testing.T) {
	shown, with := parametrized(t)
	const one = "/one/dsvs.json"
	var said []map[string]string
	p := &panel{order: []string{one}, state: map[string]attach.State{one: *shown[0].live},
		replaying: map[string]change{}, refused: map[string]string{}, replayed: make(chan replay, 4),
		done: make(chan struct{}), failed: map[string]string{}, waking: map[string]bool{},
		pending: map[string]map[string]time.Time{}}
	p.setting = func(_ string, values map[string]string) error {
		said = append(said, values)
		return nil
	}
	zonesOf := func(name string) []zone {
		canvas, zones, _ := paint(p.snapshot(), looking, with, time.Now(), p.view())
		checkZones(t, canvas, zones)
		var mine []zone
		for _, z := range zones {
			if z.name == name {
				mine = append(mine, z)
			}
		}
		return mine
	}

	segments := zonesOf("api")
	if len(segments) != 2 || segments[0].value != "staging" || segments[1].value != "demo" {
		t.Fatalf("api's segments offer %v, wanted staging and demo, not the chosen value", segments)
	}
	p.act(segments[1])
	<-p.replayed
	delete(p.replaying, one)
	if want := (map[string]string{"api": "demo"}); !reflect.DeepEqual(said, []map[string]string{want}) {
		t.Errorf("a click on the demo segment asks for %v", said)
	}

	if closed := zonesOf("catalog"); len(closed) != 1 || closed[0].do != drop {
		t.Fatalf("a select of five values offers %v, wanted a single button", closed)
	}
	p.act(zonesOf("catalog")[0])
	open := zonesOf("catalog")
	if len(open) != 6 {
		t.Fatalf("the unfolded list offers %d zones, wanted the button and five values", len(open))
	}
	p.act(open[1])
	<-p.replayed
	delete(p.replaying, one)
	if want := (map[string]string{"catalog": "empty"}); !reflect.DeepEqual(said[1], want) {
		t.Errorf("choosing a value from the list asks for %v", said[1])
	}
	if len(zonesOf("catalog")) != 1 {
		t.Error("the list stays unfolded after a value is chosen")
	}

	p.act(zonesOf("catalog")[0])
	p.act(zonesOf("catalog")[0])
	if len(zonesOf("catalog")) != 1 {
		t.Error("a second click on the button does not fold the list")
	}
}

// While a session replays, none of its parameters offers anything: a second replay cannot cross a
// first. A refusal leaves its reason under the parameters, until the next change.
func TestNothingIsOfferedWhileASessionReplays(t *testing.T) {
	shown, with := parametrized(t)
	const one = "/one/dsvs.json"
	paramZones := func(zones []zone) int {
		seen := 0
		for _, z := range zones {
			if z.do == choose || z.do == drop {
				seen++
			}
		}
		return seen
	}
	_, calm, _ := paint(shown, looking, with, time.Now(), aside)
	if paramZones(calm) == 0 {
		t.Fatal("a session replaying nothing offers no parameter")
	}

	shown[0].replaying = &change{"api", "staging"}
	canvas, zones, _ := paint(shown, looking, with, time.Now(), aside)
	checkZones(t, canvas, zones)
	if got := paramZones(zones); got != 0 {
		t.Errorf("during the replay, %d parameter zones", got)
	}
	shown[0].replaying = nil

	p := &panel{order: []string{one}, state: map[string]attach.State{one: *shown[0].live},
		replaying: map[string]change{}, refused: map[string]string{}, replayed: make(chan replay, 2),
		done: make(chan struct{}), failed: map[string]string{}, waking: map[string]bool{},
		pending: map[string]map[string]time.Time{}}
	p.setting = func(string, map[string]string) error { return errors.New(`api: "staging" is not one of local, demo`) }

	p.act(zone{do: choose, config: one, name: "api", value: "staging"})
	if p.snapshot()[0].replaying == nil {
		t.Error("the requested change is not being replayed")
	}
	p.answered(<-p.replayed)
	if p.snapshot()[0].replaying != nil {
		t.Error("the replay is over and the parameters stay inert")
	}
	refused := p.snapshot()
	if !strings.Contains(refused[0].refused, "not one of local, demo") {
		t.Errorf("the refusal's reason is %q", refused[0].refused)
	}
	before, _, _ := paint(shown, looking, with, time.Now(), aside)
	after, _, _ := paint(refused, looking, with, time.Now(), aside)
	if after.Bounds().Dy() <= before.Bounds().Dy() {
		t.Error("the refusal is not written under the parameters")
	}

	p.setting = func(string, map[string]string) error { return nil }
	p.act(zone{do: choose, config: one, name: "api", value: "demo"})
	if p.snapshot()[0].refused != "" {
		t.Error("the refusal survives the next change")
	}
	p.answered(<-p.replayed)
}

// tall is a screen height that leaves everything unfolded.
const tall = 10000

// aside is a pointer outside the panel, on a tall enough screen: nothing lights up, nothing is
// pressed, nothing is folded.
var aside = view{at: image.Pt(-1, -1), room: tall}

// anchor is the icon the panel opens from in the display tests: a 22 px square near the top of the
// screen, as in a status bar.
var anchor = image.Rect(120, 58, 142, 80)

// onDisplay mounts the demo panel on the current display and opens it against anchor: enough to
// observe the window itself, not only the rendering.
func onDisplay(t *testing.T) *panel {
	t.Helper()
	if os.Getenv("DISPLAY") == "" {
		t.Skip("no X display")
	}
	shown, with := demo(t)
	conn, err := xgb.NewConn()
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(conn.Close)
	setup := xproto.Setup(conn)
	p := &panel{conn: conn, with: with, screen: setup.DefaultScreen(conn),
		limit: int(setup.MaximumRequestLength) * 4, state: map[string]attach.State{},
		links: map[string]*attach.Link{}, reports: make(chan report, 1)}
	for _, one := range shown {
		if one.live != nil {
			p.state[one.config] = *one.live
		}
		p.order = append(p.order, one.config)
	}
	if err := p.show(anchor); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { xproto.DestroyWindow(conn, p.win) })
	return p
}

// LOOK=<folder> drops the panel's rendering in each of its states there as PNG, to judge its look
// without a screen.
func TestLookLeavesThePanelAsAPNG(t *testing.T) {
	into := os.Getenv("LOOK")
	if into == "" {
		t.Skip("LOOK=<folder> drops the rendering as PNG")
	}
	full, with := demo(t)
	found := renewal{stage: found, to: "v0.1.1-rc.1+build.2026"}
	upToDate := renewal{stage: idle, said: "up to date"}
	betaOut, betaIn := found, upToDate
	betaOut.beta, betaOut.betaOut = installBeta, "v0.2.0-beta.1"
	betaIn.beta = removeBeta

	cold := []project{{config: "/home/user/front/dsvs.json"}, {config: "/home/user/shop-api/dsvs.json"},
		{config: "/home/user/wt/feat-cart/front/dsvs.json", waking: true}}

	failing := append([]project(nil), full...)
	failing[0].failed = "config.dsvs.ts: the configuration did not load: SyntaxError: Unexpected token '}' (line 41)"
	failing[0].live = &attach.State{Config: full[0].config, Port: 4200, Apps: []attach.App{
		{Name: "front", Status: "ready", Ready: true, Since: time.Now().Add(-2 * time.Hour)},
		{Name: "admin", Status: "idle", Last: "exited (137) · killed", Fell: true},
		{Name: "docs", Status: "idle", Last: "no port after 30s", Fell: true},
	}}
	failing[2].failed = "no session: port 4200 is held by another program"

	many := make([]attach.App, 20)
	for i := range many {
		many[i] = attach.App{Name: fmt.Sprintf("service-%02d-with-a-rather-long-name", i+1), Status: "ready",
			Ready: true, Since: time.Now().Add(-time.Duration(3+i) * time.Minute)}
	}
	many[4] = attach.App{Name: many[4].Name, Status: "idle", Last: "exited (1)", Fell: true}
	many[9] = attach.App{Name: many[9].Name, Status: "starting", Since: time.Now().Add(-8 * time.Second)}
	overflowing := append([]project(nil), full...)
	overflowing[0].live = &attach.State{Config: full[0].config, Port: 4400, Apps: many,
		Facades: []attach.Facade{{Host: "localhost"}, {Host: "api.localhost"}}}

	params, _ := parametrized(t)
	inertly := append([]project(nil), params...)
	inertly[0].live = withState(params[0].live, func(s *attach.State) { s.Params = inert })
	replaying := append([]project(nil), params...)
	replaying[0].replaying = &change{"api", "staging"}
	refusing := append([]project(nil), params...)
	refusing[0].refused = `api: "staging" is not one of local, demo`

	unseen, invalid := append([]project(nil), params...), append([]project(nil), params...)
	unseen[0].live = withState(params[0].live, func(s *attach.State) { s.Observed = nil })
	invalid[0].live = withState(params[0].live, func(s *attach.State) { s.NotApplied = "locale: demo has no fr build" })
	stale := append([]project(nil), full...)
	stale[0].live = withState(full[0].live, func(s *attach.State) {
		s.NotApplied = "SyntaxError [ERR_INVALID_TYPESCRIPT_SYNTAX]: Unexpected token `,`. Expected identifier, string literal, numeric literal or [ for the computed key"
	})

	withForm := func(api string, change func([]config.Field) []config.Field) []project {
		fields := []config.Field{
			{Name: "api", Type: "choice", Choices: []string{"local", "staging", "demo"}, Value: api, Network: true},
			{Name: "deployed.version", Type: "choice", Choices: []string{"v2", "v3"}, Value: "v2", Hidden: api == "local", Keep: true},
			{Name: "deployed.traces", Type: "flag", Choices: []string{"false", "true"}, Value: "false", Hidden: api == "local",
				Disabled: true, Reasons: []string{"traces are only reachable from the company network"}},
			{Name: "products", Type: "number", Value: "20", Hidden: api != "local", Min: bound(0), Max: bound(5000)},
			{Name: "locale", Type: "choice", Choices: []string{"en", "fr"}, Value: "en", Keep: true},
			{Name: "branch", Type: "text", Value: "main"},
		}
		if change != nil {
			fields = change(fields)
		}
		return []project{{config: params[0].config, live: withState(params[0].live, func(s *attach.State) { s.Params = fields })}}
	}
	formed, local := withForm("staging", nil), withForm("local", nil)
	typed := view{at: image.Pt(-1, -1), room: tall, typing: draft{slot{local[0].config, "products"}, []rune("300"), 3}}
	overLimit := append([]project(nil), local...)
	overLimit[0].refused = "products: at most 5000"
	wrong := withForm("staging", func(fields []config.Field) []config.Field {
		fields[4].Value, fields[4].Errors = "fr", []string{"staging has no fr build"}
		return fields
	})
	wrong[0].live.Errors = []string{"staging is down for maintenance until 14:00"}
	long := withForm("staging", func(fields []config.Field) []config.Field {
		fields[2].Reasons = []string{"traces are only reachable from the company network", "and its tunnel is down since the certificate of the concentrator expired this morning"}
		fields[5].Errors = []string{"must match ^[a-z0-9-]*$: a branch name is made of lowercase letters, digits and dashes, and this one holds a slash"}
		return fields
	})
	long[0].live.NotApplied = stale[0].live.NotApplied
	long[0].live.Observed = &config.Observed{Exits: []config.Exit{{Via: "198.51.100.2", Dev: "wlan0"}},
		Links: []config.Link{{Name: "tun0"}, {Name: "wg0"}, {Name: "docker0"}, {Name: "br-5f3c2a91d0e4"}, {Name: "veth7a1b2c3"}}}

	scenes := []struct {
		name     string
		projects []project
		news     renewal
		seen     view
	}{
		{"form", formed, upToDate, aside},
		{"form-hidden-group", local, upToDate, aside},
		{"form-errors", wrong, upToDate, aside},
		{"entry", local, upToDate, typed},
		{"entry-without-keyboard", local, upToDate, view{at: image.Pt(-1, -1), room: tall, keyless: slot{local[0].config, "products"}}},
		{"entry-refused", overLimit, upToDate, aside},
		{"long-message", long, upToDate, aside},
		{"full", full, found, view{at: image.Pt(292, 52), room: tall}},
		{"empty", nil, looking, aside},
		{"cold", cold, upToDate, aside},
		{"errors", failing, failure(errors.New("update failed: git fetch: could not resolve host")), aside},
		{"overflow", overflowing, upToDate, view{at: image.Pt(-1, -1), room: 760}},
		{"params", params, upToDate, view{at: image.Pt(200, 150), room: tall}},
		{"params-replay", replaying, upToDate, aside},
		{"params-refused", refusing, upToDate, aside},
		{"params-inert", inertly, upToDate, aside},
		{"network-nothing", unseen, upToDate, aside},
		{"form-invalid", invalid, upToDate, aside},
		{"config-refused", stale, upToDate, aside},
		{"params-dropdown", params, upToDate, view{at: image.Pt(200, 330), room: tall, dropped: slot{params[0].config, "catalog"}}},
		{"beta-offered", full, betaOut, aside},
		{"beta-removal", full, betaIn, aside},
	}
	for _, scene := range scenes {
		canvas, _, _ := paint(scene.projects, scene.news, with, time.Now(), scene.seen)
		file, err := os.Create(filepath.Join(into, "panel-"+scene.name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(file, canvas); err != nil {
			t.Fatal(err)
		}
		file.Close()
	}
}

// A configuration the session does not apply is said under the project's title, and reads whole: a
// long reason takes the lines it needs, like any reason.
func TestAConfigurationNotAppliedReadsWhole(t *testing.T) {
	shown, with := demo(t)
	calm, _, _ := paint(shown, looking, with, time.Now(), aside)
	grown := func(why string) int {
		stale := append([]project(nil), shown...)
		stale[0].live = withState(shown[0].live, func(s *attach.State) { s.NotApplied = why })
		canvas, zones, _ := paint(stale, looking, with, time.Now(), aside)
		checkZones(t, canvas, zones)
		return canvas.Bounds().Dy() - calm.Bounds().Dy()
	}
	if got := grown("locale: demo has no fr build"); got != errorRow+6 {
		t.Errorf("a short reason grows the panel by %d px, wanted one line (%d)", got, errorRow+6)
	}
	if got := grown(strings.Repeat("SyntaxError: Unexpected token ", 20)); got < 8*errorRow {
		t.Errorf("600 characters of reason grow the panel by %d px: it is cut", got)
	}
}

// What the machine observes and an imposed value read in full: beside their label when they fit,
// below otherwise, over the lines they need.
func TestAValueReadsWhole(t *testing.T) {
	shown, with := parametrized(t)
	height := func(change func(*attach.State)) int {
		one := append([]project(nil), shown...)
		one[0].live = withState(shown[0].live, func(s *attach.State) {
			s.Params = nil
			change(s)
		})
		canvas, zones, _ := paint(one, looking, with, time.Now(), aside)
		checkZones(t, canvas, zones)
		return canvas.Bounds().Dy()
	}
	office := config.Exit{Via: "198.51.100.2", Dev: "wlan0"}
	calm := height(func(s *attach.State) { s.Observed = &config.Observed{Exits: []config.Exit{office}} })
	links := []config.Link{}
	for _, name := range []string{"tun0", "tun1", "wg0", "wg1", "docker0", "br-shop", "veth-shop", "veth-api"} {
		links = append(links, config.Link{Name: name})
	}
	if got := height(func(s *attach.State) { s.Observed = &config.Observed{Links: links, Exits: []config.Exit{office}} }); got < calm+2*errorRow {
		t.Errorf("a network with nine links grows the panel by %d px: it is cut", got-calm)
	}
	imposed := func(value string) func(*attach.State) {
		return func(s *attach.State) {
			s.Observed = &config.Observed{Exits: []config.Exit{office}}
			s.Params = []config.Field{{Name: "api", Type: "text", Value: value, Readonly: true}}
		}
	}
	short := height(imposed("staging"))
	if got := height(imposed(strings.Repeat("https://staging.example.test/v2 ", 4))); got < short+2*errorRow {
		t.Errorf("an imposed value of 128 characters grows the panel by %d px: it is cut", got-short)
	}
}

// The panel draws into a file without a display: with nothing known, it says "No project yet", and a
// remembered project appears there cold, as the icon would open it.
func TestThePanelDrawsIntoAPNGWithoutADisplay(t *testing.T) {
	t.Setenv("DISPLAY", ":999")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	with, err := load()
	if err != nil {
		t.Fatal(err)
	}
	drawn := func() image.Image {
		t.Helper()
		var out bytes.Buffer
		if err := Render(&out); err != nil {
			t.Fatal(err)
		}
		shown, err := png.Decode(&out)
		if err != nil {
			t.Fatal(err)
		}
		return shown
	}
	idle := renewal{stage: idle}
	empty, _, _ := paint(nil, idle, with, time.Now(), aside)
	if got := drawn(); !samePixels(got, empty) {
		t.Error("with neither session nor project, the file is not the empty panel")
	}

	config := filepath.Join(t.TempDir(), "dsvs.json")
	if err := os.WriteFile(config, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := projects.Remember(config); err != nil {
		t.Fatal(err)
	}
	cold, _, _ := paint([]project{{config: config}}, idle, with, time.Now(), aside)
	if got := drawn(); !samePixels(got, cold) {
		t.Error("a remembered project is not drawn cold")
	}
}

func samePixels(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			if color.RGBAModel.Convert(a.At(x, y)) != color.RGBAModel.Convert(b.At(x, y)) {
				return false
			}
		}
	}
	return true
}

func withState(live *attach.State, change func(*attach.State)) *attach.State {
	copied := *live
	change(&copied)
	return &copied
}

func has(deeds []deed, wanted deed) bool { return count(deeds, wanted) > 0 }

func count(deeds []deed, wanted deed) int {
	seen := 0
	for _, d := range deeds {
		if d == wanted {
			seen++
		}
	}
	return seen
}

// The panel never confiscates the input. It did once: pointer and keyboard grabs, no way out, a
// desktop to kill from dom0. This test says it in a way that cannot be bypassed — another client must
// be able to grab pointer and keyboard while the panel is open, which is impossible if the panel
// holds them.
func TestThePanelNeverHoldsTheInput(t *testing.T) {
	onDisplay(t)

	// Another client, as any desktop menu would be.
	other, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	root := xproto.Setup(other).DefaultScreen(other).Root

	pointer, err := xproto.GrabPointer(other, false, root, xproto.EventMaskButtonPress,
		xproto.GrabModeAsync, xproto.GrabModeAsync, xproto.WindowNone, xproto.CursorNone,
		xproto.TimeCurrentTime).Reply()
	if err != nil {
		t.Fatal(err)
	}
	xproto.UngrabPointer(other, xproto.TimeCurrentTime)
	if pointer.Status != xproto.GrabStatusSuccess {
		t.Errorf("the pointer is grabbed by the panel (%v)", pointer.Status)
	}

	keyboard, err := xproto.GrabKeyboard(other, false, root, xproto.TimeCurrentTime,
		xproto.GrabModeAsync, xproto.GrabModeAsync).Reply()
	if err != nil {
		t.Fatal(err)
	}
	xproto.UngrabKeyboard(other, xproto.TimeCurrentTime)
	if keyboard.Status != xproto.GrabStatusSuccess {
		t.Errorf("the keyboard is grabbed by the panel (%v)", keyboard.Status)
	}
}

// There is always a way out, whatever the panel shows: a clickable cross, which depends neither on a
// key nor on the focus.
func TestThereIsAlwaysAWayOut(t *testing.T) {
	full, with := demo(t)
	for what, shown := range map[string][]project{"full": full, "empty": nil} {
		_, zones, _ := paint(shown, looking, with, time.Now(), aside)
		var offered []deed
		for _, z := range zones {
			offered = append(offered, z.do)
		}
		if count(offered, dismiss) != 1 {
			t.Errorf("panel %s: %d crosses, wanted one", what, count(offered, dismiss))
		}
		// And the global gestures stay reachable without a project: quitting dsvs had disappeared
		// there.
		if count(offered, quitDsvs) != 1 {
			t.Errorf("panel %s: %d \"quit dsvs\", wanted one", what, count(offered, quitDsvs))
		}
	}
	// And this way out really closes: that is what the loop reads to return.
	if (&panel{}).act(zone{do: dismiss}) {
		t.Error("the cross does not close the panel")
	}
}

// Two offset monitors leave dead strips in the X screen, which is their bounding box. The panel must
// land on a monitor, entirely: placed in a dead strip, it is invisible and its cross with it — it
// happened, on exactly this geometry.
func TestThePanelLandsOnARealScreen(t *testing.T) {
	left := image.Rect(0, 0, 2560, 1440)
	right := image.Rect(2560, 240, 4480, 1440)
	screens := []image.Rectangle{right, left}
	size := image.Pt(420, 154)

	cases := map[string]image.Rectangle{
		"near the edge between the two": image.Rect(2266, 17, 2288, 39),
		"in the dead strip":             image.Rect(3000, 40, 3022, 62),
		"bottom right":                  image.Rect(4470, 1430, 4492, 1452),
		"top-left corner":               image.Rect(0, 0, 22, 22),
	}
	for what, at := range cases {
		got := beside(at, size, screens)
		panel := image.Rectangle{Min: got, Max: got.Add(size)}
		if !panel.In(left) && !panel.In(right) {
			t.Errorf("%s: panel at %v, on no monitor", what, panel)
		}
	}

	// Without RandR, only the bounding box is left: we fit into it, for lack of anything better.
	whole := []image.Rectangle{image.Rect(0, 0, 4480, 1440)}
	if got := beside(image.Rect(4470, 1430, 4492, 1452), size, whole); got != image.Pt(4056, 1272) {
		t.Errorf("without known monitors, panel at %v", got)
	}
}

// The panel opens on the anchor's side away from the status bar, and the screen edge the anchor is
// closest to is all that designates it: under Qubes the bar lives in dom0, and the VM's X publishes
// neither work area nor struts. The icon is in it, so the bar is on the side where the icon almost
// touches the edge.
func TestThePanelOpensOnTheSideAwayFromTheBar(t *testing.T) {
	screens := []image.Rectangle{image.Rect(0, 0, 1920, 1080)}
	size := image.Pt(420, 300)

	cases := map[string]struct {
		anchor image.Rectangle
		want   image.Point
	}{
		"bar at the top":    {image.Rect(900, 2, 922, 24), image.Pt(900, 28)},
		"bar at the bottom": {image.Rect(900, 1056, 922, 1078), image.Pt(900, 752)},
		"bar on the left":   {image.Rect(2, 500, 24, 522), image.Pt(28, 500)},
		"bar on the right":  {image.Rect(1896, 500, 1918, 522), image.Pt(1472, 500)},
		// Without an icon — the panel asked for from the command line — the anchor is the pointer
		// alone: an empty rectangle, which the same computation surrounds the same way.
		"at the pointer": {image.Rectangle{Min: image.Pt(700, 300), Max: image.Pt(700, 300)}, image.Pt(700, 304)},
	}
	for what, c := range cases {
		if got := beside(c.anchor, size, screens); got != c.want {
			t.Errorf("%s: panel at %v, wanted at %v", what, got, c.want)
		}
	}
}

// The click that brings up the panel must not remove it straight away; a click elsewhere, afterwards,
// must.
func TestTheOpeningClickDoesNotCloseThePanel(t *testing.T) {
	p := &panel{origin: image.Pt(100, 100), size: image.Pt(420, 154)}
	elsewhere, inside := image.Pt(2000, 900), image.Pt(150, 150)

	if p.saw(inside, true) {
		t.Error("the opening click closed the panel")
	}
	if p.saw(elsewhere, true) {
		t.Error("a button never released closed the panel")
	}
	if p.saw(elsewhere, false) {
		t.Error("releasing the button closed the panel")
	}
	if !p.saw(elsewhere, true) {
		t.Error("a click elsewhere should close the panel")
	}
	if p.saw(inside, true) {
		t.Error("a click in the panel must not close it")
	}
}

// A click only acts if released where it began: that is what leaves the right to change one's mind by
// dragging off the button, and what makes a press caught elsewhere not commit us.
func TestAClickActsWhereItBegan(t *testing.T) {
	cross := zone{at: image.Rect(390, 6, 410, 26), do: dismiss}
	p := &panel{zones: []zone{cross}}

	p.grab(400, 16)
	if !p.clicked(200, 100) {
		t.Fatal("a click dragged off the cross closed the panel")
	}

	p.grab(200, 100)
	if !p.clicked(400, 16) {
		t.Fatal("a click pressed elsewhere closed the panel when released on the cross")
	}

	p.grab(400, 16)
	if p.clicked(400, 16) {
		t.Fatal("a full click on the cross did not close the panel")
	}
}

// Pressing a button, changing one's mind and dragging out of the panel without releasing does not
// close it: the gesture is still ours. It was the tick watching the pointer that saw it as a click
// elsewhere, where the event path already allowed changing one's mind.
func TestDraggingOutOfThePanelDoesNotCloseIt(t *testing.T) {
	quit := zone{at: image.Rect(300, 120, 400, 144), do: quitDsvs}
	p := &panel{zones: []zone{quit}, origin: image.Pt(100, 100), size: image.Pt(420, 154)}
	inside, elsewhere := image.Pt(350, 230), image.Pt(2000, 900)

	if p.saw(elsewhere, false) {
		t.Fatal("releasing the button elsewhere closed the panel")
	}
	p.grab(250, 130) // the press, in window coordinates
	if p.saw(inside, true) {
		t.Error("the button pressed in the panel closed it")
	}
	if p.saw(elsewhere, true) {
		t.Error("dragging out of the panel without releasing closed it")
	}
	if !p.clicked(2000-100, 900-100) {
		t.Error("the gesture abandoned outside the panel acted")
	}
	if !p.saw(elsewhere, true) {
		t.Error("a click elsewhere, once the gesture is given up, no longer closes the panel")
	}
}

// The panel reopens on every icon click, in a service that lives for the whole session: what followed
// its sessions must leave with it, otherwise only goroutines blocked on a send nobody reads anymore
// remain.
func TestWhatFollowedTheSessionsLeavesWithThePanel(t *testing.T) {
	p := &panel{links: map[string]*attach.Link{}, reports: make(chan report), done: make(chan struct{})}
	states := make(chan attach.State, 1)
	states <- attach.State{Config: "/one/dsvs.json"}

	gone := make(chan struct{})
	go func() {
		defer close(gone)
		p.follow("/one/dsvs.json", states)
	}()

	p.release()
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("following a session survives the panel closing")
	}
}

// An update step is not lost for lack of room: the one dropped would be the last, that is, the reason
// for the failure. A closed panel, on the other hand, holds nobody back.
func TestAnUpdateStepIsNeverDropped(t *testing.T) {
	p := &panel{told: make(chan renewal), done: make(chan struct{})}

	said := make(chan struct{})
	go func() { defer close(said); p.tell(doing("building")) }()
	select {
	case news := <-p.told:
		if news.said != "building" {
			t.Errorf("the panel heard %q", news.said)
		}
	case <-said:
		t.Fatal("the step was dropped instead of waiting for the loop")
	case <-time.After(2 * time.Second):
		t.Fatal("the step never arrived")
	}

	p.release()
	freed := make(chan struct{})
	go func() { defer close(freed); p.tell(doing("failed")) }()
	select {
	case <-freed:
	case <-time.After(2 * time.Second):
		t.Fatal("the closed panel still holds back the update")
	}
}

// heard is what the panel ordered, in order: enough to judge a gesture by what it gets done, without
// real sessions to lend it.
type heard struct {
	config string
	do     deed
	name   string
}

// Each gesture goes to whom it concerns, with the verb the session understands: a "start" click on an
// app starts one and only one, "stop apps" only talks to its project, and the two global gestures
// talk to everyone.
func TestEachGestureGoesToWhoItConcerns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // "quit dsvs" must reach no real service

	const one, two, cold = "/one/dsvs.json", "/two/dsvs.json", "/cold/dsvs.json"
	var said []heard
	p := &panel{
		order:  []string{one, two, cold},
		state:  map[string]attach.State{one: {Config: one}, two: {Config: two}},
		links:  map[string]*attach.Link{one: {}, two: {}},
		failed: map[string]string{}, waking: map[string]bool{}, pending: map[string]map[string]time.Time{},
		forgot: make(chan replay, 1), done: make(chan struct{}),
	}
	p.ordered = func(config string, do deed, name string) error {
		said = append(said, heard{config, do, name})
		return nil
	}
	forgetting := func(config string) bool {
		if !p.act(zone{do: forget, config: config}) {
			return false
		}
		p.forgotten(<-p.forgot)
		return true
	}

	for _, z := range []zone{
		{do: start, config: one, name: "front"},
		{do: stop, config: one, name: "front"},
		{do: restart, config: one, name: "front"},
		{do: closeAll, config: two},
		{do: stopEverything},
	} {
		if !p.act(z) {
			t.Errorf("%v closed the panel", z.do)
		}
	}
	wanted := []heard{
		{one, "start", "front"}, {one, "stop", "front"}, {one, "restart", "front"},
		{two, "quit", ""},
	}
	if len(said) != len(wanted)+2 {
		t.Fatalf("the panel ordered %v", said)
	}
	if !slices.Equal(said[:len(wanted)], wanted) {
		t.Errorf("the panel ordered %v, wanted %v", said[:len(wanted)], wanted)
	}
	// "stop all apps" talks to each session, once, and to no other.
	for _, config := range []string{one, two} {
		if !slices.Contains(said[len(wanted):], heard{config, closeAll, ""}) {
			t.Errorf("\"stop all apps\" said nothing to %s", config)
		}
	}

	// Forgetting a cold project removes its row; an open session keeps its own.
	if !forgetting(cold) || slices.Contains(p.order, cold) {
		t.Errorf("after the forget, the rows are %v", p.order)
	}
	if !forgetting(one) || !slices.Contains(p.order, one) {
		t.Errorf("forgetting a running project removed its row: %v", p.order)
	}

	// Quitting dsvs stops the sessions, then closes the panel — the icon has the last word.
	said = nil
	if p.act(zone{do: quitDsvs}) {
		t.Error("\"quit dsvs\" left the panel open")
	}
	if len(said) != 2 {
		t.Errorf("\"quit dsvs\" ordered %v, wanted one stop per session", said)
	}
}

// An order that does not arrive reads on the project's row: a dead session's socket used to leave a
// click without the slightest answer.
func TestAnOrderThatFailsStaysOnTheProjectLine(t *testing.T) {
	const one = "/one/dsvs.json"
	p := &panel{failed: map[string]string{}, pending: map[string]map[string]time.Time{}, links: map[string]*attach.Link{one: {}}}
	p.ordered = func(string, deed, string) error { return errors.New("session is gone") }

	p.act(zone{do: stop, config: one, name: "front"})
	if p.failed[one] != "session is gone" {
		t.Errorf("the project's row says %q", p.failed[one])
	}

	p.ordered = func(string, deed, string) error { return nil }
	p.act(zone{do: stop, config: one, name: "front"})
	if p.failed[one] != "" {
		t.Errorf("after an order went through, the row still says %q", p.failed[one])
	}
}

// A terminal not found leaves its reason on the project's row, and the panel stays open: closing
// without a word was the only gesture whose failure read nowhere.
func TestAMissingTerminalLeavesItsReasonOnTheProjectLine(t *testing.T) {
	t.Setenv("PATH", "") // no terminal will be found, so no window opened
	const one = "/one/dsvs.json"
	p := &panel{failed: map[string]string{}, state: map[string]attach.State{one: {Config: one}}}

	if !p.act(zone{do: openTUI, config: one}) {
		t.Error("the panel closed on a failed opening")
	}
	if !strings.Contains(p.failed[one], "no terminal found") {
		t.Errorf("the project's row says %q", p.failed[one])
	}
}

// A long failure reason reads whole: it takes the lines it needs, rather than ending in an ellipsis
// at the panel's width. It is all there is to know why a project did not start.
func TestALongReasonTakesSeveralLines(t *testing.T) {
	full, with := demo(t)
	short := append([]project(nil), full...)
	short[2].failed = "no port"
	long := append([]project(nil), full...)
	long[2].failed = "config.dsvs.ts: no facade declared; missing port: it is the only open port, and it is declared at the top of the configuration"

	brief, _, _ := paint(short, looking, with, time.Now(), aside)
	whole, _, _ := paint(long, looking, with, time.Now(), aside)
	if whole.Bounds().Dy() <= brief.Bounds().Dy() {
		t.Errorf("the panel is %d pixels high with a long reason, %d with a short one",
			whole.Bounds().Dy(), brief.Bounds().Dy())
	}
}

// The canvas is exactly the size of what was painted on it: the last row of actions must be whole in
// it, whatever the combination. It is the safety net of a geometry written only once — a disagreement
// would silently crop what is lowest, that is, the global gestures.
func TestTheCanvasHoldsTheLastThingPainted(t *testing.T) {
	full, with := demo(t)
	failing := append([]project(nil), full...)
	failing[2].failed = "no session serves this project"
	waking := append([]project(nil), full...)
	waking[2].waking = true

	shows := map[string][]project{"empty": nil, "full": full, "failing": failing, "waking": waking}
	news := map[string]renewal{
		"working": doing("building"),
		"idle":    {stage: idle, said: "up to date"},
		"found":   {stage: found, to: "v1.4.0"},
		"looking": looking,
	}
	for what, shown := range shows {
		for when, said := range news {
			canvas, zones, _ := paint(shown, said, with, time.Now(), aside)
			for _, z := range zones {
				if !z.at.In(canvas.Bounds()) {
					t.Errorf("%s, %s: zone %v overflows the canvas %v", what, when, z.at, canvas.Bounds())
				}
			}
			out := zoneOf(t, zones, quitDsvs)
			if got := canvas.RGBAAt(out.Min.X+2, out.Max.Y-2); got != key {
				t.Errorf("%s, %s: the bottom of the last button is %v, wanted a button's color", what, when, got)
			}
		}
	}
}

func zoneOf(t *testing.T, zones []zone, wanted deed) image.Rectangle {
	t.Helper()
	for _, z := range zones {
		if z.do == wanted {
			return z.at
		}
	}
	t.Fatalf("no zone %v", wanted)
	return image.Rectangle{}
}

// Nothing spills into the margins: a path, a host, an app name or a version found come from elsewhere
// and have no promised length. Both margins are checked at once — a text that touches them is a text
// that runs under another column, or out of the panel. A long version goes through a button's label,
// the only path that reached the left margin.
func TestNothingSpillsIntoTheMargin(t *testing.T) {
	_, with := demo(t)
	long := strings.Repeat("interminable-", 12)
	state := attach.State{
		Config: "/" + long + "/dsvs.json", Port: 4200,
		Apps: []attach.App{{Name: long, Status: "ready", Ready: true, Since: time.Now()}},
		Facades: []attach.Facade{{Host: long + ".localhost"}, {Host: long + ".admin.localhost"},
			{Host: long + ".docs.localhost"}},
	}
	shown := []project{{config: state.Config, live: &state, failed: long}, {config: "/" + long + "/two/dsvs.json"}}

	for what, news := range map[string]renewal{"a step": doing(long), "a version": {stage: found, to: long}} {
		canvas, _, _ := paint(shown, news, with, time.Now(), aside)
		for _, margin := range []struct{ from, to int }{{0, pad}, {width - pad, width}} {
			for x := margin.from; x < margin.to; x++ {
				for y := range canvas.Bounds().Dy() {
					if got := canvas.RGBAAt(x, y); got != ground {
						t.Fatalf("%s: ink at %d,%d in the margin: %v", what, x, y, got)
					}
				}
			}
		}
	}
}

// Pointing at an app row lights that one, and not its neighbor: rows do not start at the top of the
// panel, and a window that redid their arithmetic would lag by a few pixels.
func TestTheHoverLightsTheRowUnderThePointer(t *testing.T) {
	shown, with := demo(t)
	_, _, rows := paint(shown, looking, with, time.Now(), aside)
	if len(rows) < 2 {
		t.Fatalf("%d app rows painted", len(rows))
	}

	for lit := range rows {
		at := image.Pt(rows[lit].Min.X+2, rows[lit].Min.Y+rowHeight/2)
		canvas, _, again := paint(shown, looking, with, time.Now(), view{at: at, room: tall})
		if !slices.Equal(again, rows) {
			t.Fatalf("the rows move on hover: %v then %v", rows, again)
		}
		if rowAt(rows, at) != lit {
			t.Fatalf("the pointer at %v is on row %d, wanted %d", at, rowAt(rows, at), lit)
		}
		for i, row := range rows {
			got := canvas.RGBAAt(row.Min.X+1, row.Min.Y+rowHeight/2)
			if wanted := ground; i != lit && got != wanted {
				t.Errorf("hovering row %d also lights row %d (%v)", lit, i, got)
			}
			if got != rowLit && i == lit {
				t.Errorf("hovering row %d does not light it (%v)", lit, got)
			}
		}
	}

	// Outside any row, none is lit.
	if got := rowAt(rows, image.Pt(2, 2)); got != -1 {
		t.Errorf("the panel's corner is on row %d", got)
	}
}

// A color touched up later must not silently break readability: every text/background or
// glyph/background pair of the palette keeps its WCAG 2.1 AA contrast, 4.5 for a text and 3 for a
// glyph or a control.
func TestThePaletteKeepsItsContrasts(t *testing.T) {
	const text, shape = 4.5, 3.0
	for _, c := range []struct {
		what   string
		on, in color.RGBA
		least  float64
	}{
		{"ink/ground", ink, ground, text},
		{"ink/rowLit", ink, rowLit, text},
		{"ink/key", ink, key, text},
		{"ink/keyLit", ink, keyLit, text},
		{"ink/keyDown", ink, keyDown, text},
		{"faint/ground", faint, ground, text},
		{"faint/rowLit", faint, rowLit, text},
		{"subdued/key", subdued, key, text},
		{"brokenText/ground", brokenText, ground, text},
		{"brokenText/rowLit", brokenText, rowLit, text},
		{"white/accent", white, accent, text},
		{"white/accentLit", white, accentLit, text},
		{"white/accentDown", white, accentDown, text},
		{"accent/ground", accent, ground, shape},
		{"ready/ground", ready, ground, shape},
		{"ready/rowLit", ready, rowLit, shape},
		{"waking/ground", waking, ground, text},
		{"waking/rowLit", waking, rowLit, shape},
		{"broken/ground", broken, ground, shape},
		{"broken/rowLit", broken, rowLit, shape},
		{"asleep/ground", asleep, ground, shape},
		{"asleep/rowLit", asleep, rowLit, shape},
		{"branch/ground", branch, ground, shape},
	} {
		if got := contrast(c.on, c.in); got < c.least {
			t.Errorf("%s: contrast %.2f, wanted at least %.1f", c.what, got, c.least)
		}
	}
}

// contrast is the WCAG 2.1 relative luminance ratio between two sRGB colors.
func contrast(a, b color.RGBA) float64 {
	high, low := luminance(a), luminance(b)
	if low > high {
		high, low = low, high
	}
	return (high + 0.05) / (low + 0.05)
}

func luminance(c color.RGBA) float64 {
	linear := func(v uint8) float64 {
		x := float64(v) / 255
		if x <= 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(c.R) + 0.7152*linear(c.G) + 0.0722*linear(c.B)
}

// A glyph says the state by its shape, and the color only repeats it: the four silhouettes must stay
// distinct once the color is removed, otherwise a theme that swallows a hue erases the state.
func TestTheFourStatesHaveFourSilhouettes(t *testing.T) {
	_, with := demo(t)
	state := attach.State{Config: "/one/dsvs.json", Port: 4200, Apps: []attach.App{
		{Name: "up", Status: "ready", Ready: true},
		{Name: "coming", Status: "starting"},
		{Name: "down", Status: "idle", Last: "exited (2)", Fell: true},
		{Name: "off", Status: "idle"},
	}}
	canvas, _, rows := paint([]project{{config: state.Config, live: &state}}, looking, with, time.Now(), aside)

	silhouettes := make([][]bool, len(rows))
	for i, row := range rows {
		for y := row.Min.Y; y < row.Max.Y; y++ {
			for x := 0; x < nameX-4; x++ {
				silhouettes[i] = append(silhouettes[i], canvas.RGBAAt(x, y) != ground)
			}
		}
	}
	for i := range silhouettes {
		for j := i + 1; j < len(silhouettes); j++ {
			differing := 0
			for k := range silhouettes[i] {
				if silhouettes[i][k] != silhouettes[j][k] {
					differing++
				}
			}
			if differing < 10 {
				t.Errorf("glyphs %d and %d differ by only %d pixels", i, j, differing)
			}
		}
	}
}

// An app whose order has left offers nothing anymore: the first pictogram changes meaning with the
// state, and a second click would do the opposite of what one expects. It says so in place of its
// state.
func TestAnAppWaitingForItsOrderOffersNothing(t *testing.T) {
	shown, with := demo(t)
	shown[0].pending = map[string]time.Time{"front": time.Now().Add(patience)}
	_, zones, _ := paint(shown, looking, with, time.Now(), aside)

	for _, z := range zones {
		if z.name == "front" {
			t.Errorf("a waiting app offers %v", z.do)
		}
	}
	if !has(deedsOf(zones, "docs"), stop) {
		t.Error("the project's other apps lost their gestures")
	}
}

// On a pictogram, the state column writes what the click will do: it is the tooltip the panel does
// not have. On the row alone, it keeps its state.
func TestHoveringAPictogramSaysWhatItDoes(t *testing.T) {
	shown, with := demo(t)
	_, zones, rows := paint(shown, looking, with, time.Now(), aside)
	var stopping zone
	for _, z := range zones {
		if z.name == "front" && z.do == stop {
			stopping = z
		}
	}
	row := rows[rowAt(rows, stopping.at.Min)]
	inkInStatus := func(at image.Point) int {
		canvas, _, _ := paint(shown, looking, with, time.Now(), view{at: at, room: tall})
		seen := 0
		for y := row.Min.Y; y < row.Max.Y; y++ {
			for x := statusX; x < statusX+60; x++ {
				if canvas.RGBAAt(x, y) == ink {
					seen++
				}
			}
		}
		return seen
	}
	if inkInStatus(image.Pt(row.Min.X+2, row.Min.Y+2)) != 0 {
		t.Error("the state column is in ink without the pointer on a pictogram")
	}
	if inkInStatus(stopping.at.Min.Add(image.Pt(3, 3))) == 0 {
		t.Error("the state column does not write what the click would do")
	}
}

// A gesture held on its pictogram presses it; dragged elsewhere, the background comes back.
func TestAPressedPictogramSinksUntilYouDragAway(t *testing.T) {
	shown, with := demo(t)
	_, zones, _ := paint(shown, looking, with, time.Now(), aside)
	var stopping zone
	for _, z := range zones {
		if z.name == "front" && z.do == stop {
			stopping = z
		}
	}
	sample := func(seen view) color.RGBA {
		canvas, _, _ := paint(shown, looking, with, time.Now(), seen)
		return canvas.RGBAAt(stopping.at.Min.X+3, stopping.at.Min.Y+stopping.at.Dy()/2)
	}
	on := stopping.at.Min.Add(image.Pt(3, 3))
	if got := sample(view{at: on, room: tall}); got != keyLit {
		t.Errorf("hovered: %v, wanted keyLit", got)
	}
	if got := sample(view{at: on, room: tall, pressed: stopping}); got != keyDown {
		t.Errorf("pressed: %v, wanted keyDown", got)
	}
	if got := sample(view{at: image.Pt(2, 2), room: tall, pressed: stopping}); got == keyDown {
		t.Error("dragged elsewhere, the pictogram stays pressed")
	}
}

func deedsOf(zones []zone, name string) []deed {
	var out []deed
	for _, z := range zones {
		if z.name == name {
			out = append(out, z.do)
		}
	}
	return out
}

// An order that left leaves its app waiting; an order that did not leave does not: its reason is on
// the project's row, and nothing waits for an answer that will not come.
func TestAnOrderLeavesItsAppWaitingOnlyIfItWentOut(t *testing.T) {
	const one = "/one/dsvs.json"
	p := &panel{order: []string{one}, state: map[string]attach.State{one: {Config: one}},
		failed: map[string]string{}, waking: map[string]bool{}, pending: map[string]map[string]time.Time{},
		links: map[string]*attach.Link{one: {}}}

	p.ordered = func(string, deed, string) error { return errors.New("session is gone") }
	p.act(zone{do: stop, config: one, name: "front"})
	if p.snapshot()[0].pending["front"].After(time.Now()) {
		t.Error("a refused order leaves the app waiting")
	}

	p.ordered = func(string, deed, string) error { return nil }
	p.act(zone{do: stop, config: one, name: "front"})
	if !p.snapshot()[0].pending["front"].After(time.Now()) {
		t.Error("an order that left does not put the app on hold")
	}
}

// A folded project hides its failure reason: its summary row carries the fallen state's cross, so
// that one knows there is something to read by unfolding it.
func TestAFoldedProjectStillSaysItHasFailed(t *testing.T) {
	_, with := demo(t)
	many := make([]attach.App, 20)
	for i := range many {
		many[i] = attach.App{Name: fmt.Sprintf("service-%02d", i), Status: "ready", Ready: true}
	}
	sessions := []attach.State{
		{Config: "/a/dsvs.json", Port: 4300, Apps: many},
		{Config: "/b/dsvs.json", Port: 4310, Apps: []attach.App{{Name: "front", Status: "ready", Ready: true}}},
	}
	shown := []project{{config: sessions[0].Config, live: &sessions[0]}, {config: sessions[1].Config, live: &sessions[1]}}
	folded := view{at: aside.at, room: 760, unfolded: "/b/dsvs.json"}

	summaryRed := func() int {
		canvas, zones, _ := paint(shown, looking, with, time.Now(), folded)
		summary := zoneOf(t, zones, unfold)
		seen := 0
		for y := summary.Min.Y; y < summary.Max.Y; y++ {
			for x := summary.Min.X; x < summary.Max.X; x++ {
				if canvas.RGBAAt(x, y) == broken {
					seen++
				}
			}
		}
		return seen
	}
	calm := summaryRed()
	shown[0].failed = "no port after 30s"
	if summaryRed() <= calm {
		t.Error("a failing project's summary does not stand out from a calm project")
	}
	shown[0].failed, sessions[0].NotApplied = "", "locale: demo has no fr build"
	if summaryRed() <= calm {
		t.Error("the summary of a session that does not apply its configuration does not stand out from a calm project")
	}
}

// An order that changes nothing causes no new state: the app does not wait forever, and gets its
// gestures back at the deadline.
func TestAnAppStopsWaitingWhenNoStateComes(t *testing.T) {
	const one = "/one/dsvs.json"
	p := &panel{order: []string{one}, state: map[string]attach.State{one: {Config: one}},
		failed: map[string]string{}, waking: map[string]bool{}, pending: map[string]map[string]time.Time{},
		links: map[string]*attach.Link{one: {}}}
	p.ordered = func(string, deed, string) error { return nil }
	p.act(zone{do: start, config: one, name: "front"})

	if p.expire(time.Now()) {
		t.Error("the wait ended before its deadline")
	}
	if !p.snapshot()[0].pending["front"].After(time.Now()) {
		t.Fatal("the app does not wait for its order")
	}
	if !p.expire(time.Now().Add(patience + time.Second)) {
		t.Error("past the deadline, the wait does not say it is over")
	}
	if p.snapshot()[0].pending["front"].After(time.Now().Add(patience + time.Second)) {
		t.Error("the app still waits after the deadline")
	}
	if len(p.pending) != 0 {
		t.Errorf("waits remain: %v", p.pending)
	}
}

// A sleeping project fits on one line: start and forget are on its title line, and there is nothing
// else to say about it.
func TestAColdProjectFitsOnOneLine(t *testing.T) {
	_, with := demo(t)
	cold := []project{{config: "/one/dsvs.json"}}
	_, zones, _ := paint(cold, looking, with, time.Now(), aside)

	var mine []zone
	for _, z := range zones {
		if z.config != "" {
			mine = append(mine, z)
		}
	}
	if len(mine) != 2 || !has(deedsOf(mine, ""), open) || !has(deedsOf(mine, ""), forget) {
		t.Fatalf("a cold project offers %v, wanted start and forget", mine)
	}
	if mine[0].at.Min.Y != mine[1].at.Min.Y {
		t.Errorf("start at %v and forget at %v are not on the same row", mine[0].at, mine[1].at)
	}
}

// An update's failure reads as an error, not as news: the color is what tells them apart from "up to
// date".
func TestAFailedUpdateReadsAsAnError(t *testing.T) {
	_, with := demo(t)
	inRed := func(news renewal) bool {
		canvas, _, _ := paint(nil, news, with, time.Now(), aside)
		return slices.Contains(colorsOf(canvas), brokenText)
	}
	if !inRed(failure(errors.New("git fetch: could not resolve host"))) {
		t.Error("the update's failure is not written in the error color")
	}
	if inRed(renewal{stage: idle, said: "up to date"}) {
		t.Error("an up-to-date update is written in the error color")
	}
}

func colorsOf(canvas *image.RGBA) []color.RGBA {
	var out []color.RGBA
	for i := 0; i < len(canvas.Pix); i += 4 {
		out = append(out, color.RGBA{canvas.Pix[i], canvas.Pix[i+1], canvas.Pix[i+2], canvas.Pix[i+3]})
	}
	return out
}

// Where an app listens when it starts without listening at its address reads in full under its row:
// the state column would only have room for its beginning.
func TestAnAppListeningElsewhereSaysWhereUnderItsRow(t *testing.T) {
	_, with := demo(t)
	height := func(astray string) int {
		state := attach.State{Config: "/one/dsvs.json", Port: 4200, Apps: []attach.App{{Name: "front", Status: "starting", Astray: astray}}}
		canvas, _, _ := paint([]project{{config: state.Config, live: &state}}, looking, with, time.Now(), aside)
		return canvas.Bounds().Dy()
	}
	if quiet, said := height(""), height("listens on 127.0.0.1:5173, not on 10.77.0.3:4123"); said <= quiet {
		t.Errorf("the panel is %d pixels with the address, %d without: it is not written", said, quiet)
	}
}

// When the screen is too short, the panel folds: a single project stays unfolded, and it is the one
// where something fell — it is the one we came to see. A click on a summary chooses another.
func TestThePanelFoldsWhenTheScreenIsTooShort(t *testing.T) {
	_, with := demo(t)
	many := make([]attach.App, 20)
	for i := range many {
		many[i] = attach.App{Name: fmt.Sprintf("service-%02d", i), Status: "ready", Ready: true}
	}
	sessions := []attach.State{
		{Config: "/a/dsvs.json", Port: 4300, Apps: many},
		{Config: "/b/dsvs.json", Port: 4310, Apps: []attach.App{
			{Name: "front", Status: "ready", Ready: true}, {Name: "admin", Status: "idle", Last: "exited (2)", Fell: true}}},
		{Config: "/c/dsvs.json", Port: 4320, Apps: []attach.App{{Name: "front", Status: "ready", Ready: true}}},
	}
	var shown []project
	for i := range sessions {
		shown = append(shown, project{config: sessions[i].Config, live: &sessions[i]})
	}
	const room = 760

	unfolded := func(seen view) []string {
		canvas, zones, _ := paint(shown, looking, with, time.Now(), seen)
		if canvas.Bounds().Dy() > room {
			t.Errorf("the panel is %d pixels high, the screen leaves %d", canvas.Bounds().Dy(), room)
		}
		folded := map[string]bool{}
		for _, z := range zones {
			if z.do == unfold {
				folded[z.config] = true
			}
		}
		var open []string
		for _, one := range shown {
			if !folded[one.config] {
				open = append(open, one.config)
			}
		}
		return open
	}

	if got := unfolded(view{at: aside.at, room: room}); !slices.Equal(got, []string{"/b/dsvs.json"}) {
		t.Errorf("unfolded projects: %v, wanted the one with a fallen app", got)
	}
	if got := unfolded(view{at: aside.at, room: room, unfolded: "/c/dsvs.json"}); !slices.Equal(got, []string{"/c/dsvs.json"}) {
		t.Errorf("unfolded projects: %v, wanted the chosen one", got)
	}

	offered := renewal{stage: idle, said: "up to date", beta: installBeta, betaOut: "v0.2.0-beta.1"}
	if canvas, _, _ := paint(shown, offered, with, time.Now(), view{at: aside.at, room: room}); canvas.Bounds().Dy() > room {
		t.Errorf("with the beta's row, the panel is %d pixels high, the screen leaves %d", canvas.Bounds().Dy(), room)
	}

	_, zones, _ := paint(shown, looking, with, time.Now(), aside)
	if slices.ContainsFunc(zones, func(z zone) bool { return z.do == unfold }) {
		t.Error("the panel folds while the screen leaves room")
	}

	clicked := &panel{}
	if !clicked.act(zone{do: unfold, config: "/c/dsvs.json"}) || clicked.view().unfolded != "/c/dsvs.json" {
		t.Error("a click on a summary does not choose the project to unfold")
	}
}

// A branch binary is recognized at a glance from the header: the violet square there is that of its
// icon's bars. The test binary is not called dsvs, so it is one.
func TestABranchBinaryMarksItsHeader(t *testing.T) {
	_, with := demo(t)
	canvas, _, _ := paint(nil, looking, with, time.Now(), aside)
	if got := canvas.At(pad+5, 18); got != branch {
		t.Errorf("the header's square is %v, wanted %v", got, branch)
	}
}

// The icon says what the sessions say together, with the same shape the panel draws in front of each
// app: the worst wins, and a cross wins over a half disc.
func TestTheSessionsSayTheWorstFormTogether(t *testing.T) {
	ready := attach.App{Name: "front", Status: "ready", Ready: true}
	booting := attach.App{Name: "api", Status: "starting"}
	leaving := attach.App{Name: "admin", Status: "stopping", Ready: true}
	fallen := attach.App{Name: "docs", Status: "idle", Last: "exited (1)", Fell: true}
	stopped := attach.App{Name: "old", Status: "idle", Last: "stopped"}
	done := attach.App{Name: "job", Status: "idle", Last: "exited (0)"}
	of := func(apps ...attach.App) attach.State { return attach.State{Apps: apps} }

	for _, c := range []struct {
		what   string
		states []attach.State
		want   Mood
	}{
		{"no session", nil, Idle},
		{"a session without an app", []attach.State{of()}, Idle},
		{"nothing running", []attach.State{of(stopped)}, Idle},
		{"an app that exited on its own with code 0", []attach.State{of(done, stopped)}, Idle},
		{"everything is ready", []attach.State{of(ready), of(ready, stopped)}, Running},
		{"an app starts", []attach.State{of(ready), of(booting)}, Starting},
		{"an app stops, still answering", []attach.State{of(ready, leaving)}, Starting},
		{"an app falls and one starts", []attach.State{of(booting, ready), of(fallen)}, Failed},
	} {
		if got := Overall(c.states); got != c.want {
			t.Errorf("%s: %d, wanted %d", c.what, got, c.want)
		}
	}
}
