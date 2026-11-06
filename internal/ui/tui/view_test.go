package tui

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"dsvs/internal/config"
	"dsvs/internal/core"
)

// A frame must fill the terminal exactly: a line too wide shifts everything else, and nothing
// reports it at runtime.
func TestFrameFillsTheTerminal(t *testing.T) {
	t.Parallel()
	base := demo(t)
	sizes := [][2]int{{96, 24}, {120, 40}, {200, 60}, {80, 10}, {72, 20}, {40, 12}, {26, 8}}

	for _, size := range sizes {
		width, height := size[0], size[1]
		for _, variant := range variants(t, base, width, height) {
			lines := strings.Split(variant.frame, "\n")
			if len(lines) != height {
				t.Errorf("%dx%d %s: %d lines", width, height, variant.name, len(lines))
			}
			for i, line := range lines {
				if got := ansi.StringWidth(line); got != width {
					t.Errorf("%dx%d %s: line %d is %d wide\n%s", width, height, variant.name, i, got, ansi.Strip(line))
					break
				}
			}
		}
	}
}

func TestNavigation(t *testing.T) {
	t.Parallel()
	m := resize(demo(t), 120, 30)

	// The cursor moves without taking the log along.
	m = press(m, "down", "down", "down")
	if m.cursor != 3 || m.selected != 0 {
		t.Fatalf("cursor=%d selected=%d, want 3 and 0", m.cursor, m.selected)
	}

	// First →: choose what to watch, without leaving the list.
	m = press(m, "right")
	if m.selected != 3 || m.focus != onList {
		t.Fatalf("after →: selected=%d focus=%v, want 3 and the list", m.selected, m.focus)
	}
	// Second → on the same item: enter it.
	if m = press(m, "right"); m.focus != onLog {
		t.Fatal("a second → should give focus to the log")
	}
	// Enter does exactly the same.
	m = press(m, "left", "up", "enter")
	if m.selected != 2 || m.focus != onList {
		t.Fatalf("after ⏎: selected=%d focus=%v, want 2 and the list", m.selected, m.focus)
	}
	if m = press(m, "enter"); m.focus != onLog {
		t.Fatal("a second ⏎ should give focus to the log")
	}

	// space selects without leaving the list.
	m = press(m, "left", "down", "space")
	if m.selected != 3 || m.focus != onList {
		t.Fatalf("after space: selected=%d focus=%v, want 3 and the list", m.selected, m.focus)
	}

	// esc closes the floating panel before giving focus back.
	m = press(m, "right", "i")
	if m.modal != infoModal {
		t.Fatal("i did not open the info panel")
	}
	if m = press(m, "esc"); m.modal != noModal || m.focus != onLog {
		t.Fatalf("esc should close the info without leaving the log: modal=%v focus=%v", m.modal, m.focus)
	}
	if m = press(m, "esc"); m.focus != onList {
		t.Fatal("a second esc should give focus back to the list")
	}

	// When narrow, focus alone decides the screen displayed: one of the two panels, never both.
	// Section titles only exist in the list.
	narrow := press(resize(m, 60, 20), "right")
	if got := ansi.Strip(narrow.View().Content); strings.Contains(got, "apps") {
		t.Errorf("when narrow, → should show the log alone:\n%s", got)
	}
	if got := ansi.Strip(press(narrow, "esc").View().Content); !strings.Contains(got, "apps") {
		t.Errorf("when narrow, esc should go back to the list:\n%s", got)
	}
}

// The list holds the session's port, the only thing worth seeing without opening anything, and its
// sections sort the items by what they are.
func TestListShowsThePortAndItsSections(t *testing.T) {
	t.Parallel()
	m := resize(demo(t), 120, 30)
	rows, _ := m.listRows(m.sidebarWidth(), true)
	list := ansi.Strip(strings.Join(rows, "\n"))

	for _, want := range []string{"session", "port 4300", "apps", "localhost", "/api", "staging.example.com"} {
		if !strings.Contains(list, want) {
			t.Errorf("the list should hold %q:\n%s", want, list)
		}
	}
	// A redirect is not driven: it has nothing to do in the list.
	if strings.Contains(list, "to /shop-beta/") {
		t.Errorf("redirects should stay in the card:\n%s", list)
	}
	// Apps come before facades: they are what is started first.
	if index(t, m, "front") > index(t, m, "localhost") {
		t.Error("apps should come before facades in the list")
	}
	// An app does not say who serves it, the link is circumstantial.
	app := ansi.Strip(m.listRow(index(t, m, "front"), m.sidebarWidth(), true))
	if strings.Contains(app, "/shop-beta") {
		t.Errorf("an app's line should not hold a facade path: %q", app)
	}
	// And the cursor does not stop on routes: their log would be their facade's.
	for _, it := range m.items {
		if strings.HasPrefix(it.name, "/") {
			t.Errorf("route %q should not be a list item", it.name)
		}
	}
}

func TestFilterKeepsMatchingLines(t *testing.T) {
	t.Parallel()
	m := at(t, resize(demo(t), 120, 30), "front")
	app := m.session.Apps()[0]
	app.Log.Add("bundle generation complete")
	app.Log.Add("ERROR TS2339")

	m = press(m, "/")
	for _, r := range "error" {
		m = press(m, string(r))
	}
	m = press(m, "enter")

	if m.filter != "error" {
		t.Fatalf("filter = %q", m.filter)
	}
	lines := m.lines()
	if len(lines) != 1 || !strings.Contains(lines[0].Text, "TS2339") {
		t.Fatalf("the filter should keep only one line, returned %d", len(lines))
	}
	if m = press(m, "c"); m.filter != "" {
		t.Error("c should clear the filter")
	}
}

func TestQuitAsksBeforeStopping(t *testing.T) {
	t.Parallel()
	m := press(resize(demo(t), 96, 20), "q")
	if m.modal != confirmModal {
		t.Fatal("q should ask for confirmation")
	}
	if frame := ansi.Strip(m.View().Content); !strings.Contains(frame, "quit?") {
		t.Errorf("the modal should ask the question:\n%s", frame)
	}
	// A refusal leaves everything in place.
	if refused := press(m, "n"); refused.modal != noModal {
		t.Fatal("n should cancel leaving")
	}
	if app := m.session.Apps()[0].Snapshot(); app.Status == core.Idle {
		t.Error("the question should stop nothing before being answered")
	}
	// Focus moves to the buttons, and Enter then applies to the one that has focus.
	if cancelled := press(m, "right", "enter"); cancelled.modal != noModal {
		t.Errorf("Enter on Cancel should close the modal, modal=%v", cancelled.modal)
	}

	m = press(m, "o")
	if m.modal != quittingModal {
		t.Fatal("o should confirm leaving")
	}
	frame := ansi.Strip(m.View().Content)
	if !strings.Contains(frame, "stopping the apps") {
		t.Errorf("the stopping screen should say so:\n%s", frame)
	}
	// As long as an app has not come back to a stop, the interface does not give the terminal back.
	for _, app := range m.session.Apps() {
		if app.Snapshot().Status != core.Idle {
			if _, cmd := m.Update(tickMsg(time.Now())); cmd == nil {
				t.Fatal("the interface let go of the terminal before the apps stopped")
			}
			return
		}
	}
}

// The info panel describes what focus designates, not what the log displays.
func TestInfoFollowsTheFocus(t *testing.T) {
	t.Parallel()
	m := resize(demo(t), 120, 30)

	// Cursor on admin, log left on « all »: the info must talk about admin.
	m.cursor = index(t, m, "admin")
	m = press(m, "i")
	if card := ansi.Strip(strings.Join(boxOf(t, m), "\n")); !strings.Contains(card, "sh -c exit 2") || strings.Contains(card, "sleep 300") {
		t.Errorf("the info should describe admin, under the cursor:\n%s", card)
	}

	// Once in the log, it talks about what is displayed there.
	m = press(m, "esc", "right", "i")
	card := ansi.Strip(strings.Join(boxOf(t, m), "\n"))
	if !strings.Contains(card, "exited (2)") {
		t.Errorf("the info should describe the displayed app:\n%s", card)
	}
	// An app has nothing to say about where it is served: the facade decides that.
	if strings.Contains(card, "http://") {
		t.Errorf("an app's card should not hold an address:\n%s", card)
	}
}

// A facade says where it listens: its origin, without a path — the path belongs to the route, not
// to the facade. And the screen has no banner: the panels start on the first line.
func TestFacadeSaysWhereItListens(t *testing.T) {
	t.Parallel()
	m := at(t, resize(demo(t), 120, 30), "localhost")

	card := ansi.Strip(strings.Join(boxOf(t, press(m, "i")), "\n"))
	if !strings.Contains(card, "http://localhost:4300 ·") {
		t.Errorf("the card should hold the facade's origin:\n%s", card)
	}
	if strings.Contains(card, "http://localhost:4300/") {
		t.Errorf("the origin should hold no path, it would be arbitrary:\n%s", card)
	}
	// A facade has no state of its own: it is served as long as the session runs, and its log
	// counts requests rather than announcing a listener.
	if !strings.Contains(card, "served as long as the session runs") {
		t.Errorf("the card should say what makes it reachable:\n%s", card)
	}
	if head := ansi.Strip(m.logHead(0, true)); !strings.Contains(head, "127 req") || strings.Contains(head, "http") {
		t.Errorf("the log should count requests, not hold an address: %q", head)
	}
	if top := listTop(m); !strings.Contains(top, "session") {
		t.Errorf("the list should start with the session: %q", top)
	}
}

// The modal has the keyboard: nothing gets behind it, except what it knows how to do.
func TestModalCapturesKeys(t *testing.T) {
	t.Parallel()
	m := press(at(t, resize(demo(t), 120, 30), "front"), "right") // front's log, focused
	before := m
	opened := press(m, "i")

	// The application's commands do not reach it.
	for _, key := range []string{"x", "r", "s", "b", "/", "o", "G", "left", "right"} {
		after := press(opened, key)
		if after.modal != infoModal {
			t.Errorf("%q closed the modal", key)
		}
		if after.sidebar != opened.sidebar || after.input != noInput || after.filter != opened.filter || after.offset != opened.offset || after.focus != opened.focus {
			t.Errorf("%q acted behind the modal", key)
		}
	}

	// ↑↓ scroll through the cards without closing it.
	scrolled := press(opened, "down")
	if scrolled.modal != infoModal || scrolled.cursor != opened.cursor+1 {
		t.Fatalf("↓ should move to the next card: cursor %d → %d", opened.cursor, scrolled.cursor)
	}
	if name := m.items[scrolled.cursor].name; !strings.Contains(ansi.Strip(scrolled.View().Content), name) {
		t.Errorf("the modal should describe %q after scrolling", name)
	}
	if m = press(scrolled, "esc"); m.modal != noModal {
		t.Error("esc should close the modal")
	}
	if before.focus != m.focus {
		t.Error("the modal changed the focus when closing")
	}
}

// On a lent terminal, the session outlives the TUI: the confirmation then offers to leave the TUI
// alone, that is its default, and the apps do not stop.
func TestQuitOnALentTerminalLeavesTheAppsRunning(t *testing.T) {
	t.Parallel()
	stopped := false
	lent := newModel(demo(t).session, func() { stopped = true }, true)
	m := press(resize(lent, 96, 24), "q")

	frame := ansi.Strip(m.View().Content)
	for _, label := range []string{"Cancel", "Close everything", "Leave the TUI"} {
		if !strings.Contains(frame, label) {
			t.Errorf("button %q is missing:\n%s", label, frame)
		}
	}
	if m.button != m.byDefault() {
		t.Fatalf("default button = %d, want the last one (%d)", m.button, m.byDefault())
	}
	if press(m, "enter"); stopped {
		t.Error("leaving the TUI stopped the apps")
	}
	// Space confirms like Enter.
	if press(m, "space"); stopped {
		t.Error("space on the default button stopped the apps")
	}
	// An arrow goes its own way: left of the default is « Close everything », then « Cancel ».
	if left := press(m, "left"); left.button != m.byDefault()-1 {
		t.Errorf("left leads to button %d, want %d", left.button, m.byDefault()-1)
	}
	if press(m, "left", "enter"); !stopped {
		t.Error("« Close everything » did not stop the apps")
	}
}

// The modal does not let its text touch the border.
func TestModalKeepsItsMargins(t *testing.T) {
	t.Parallel()
	for _, keys := range [][]string{{"i"}, {"?"}, {"down", "right", "i"}, {"q"}} {
		m := press(resize(demo(t), 110, 28), keys...)
		box := boxOf(t, m)
		for _, edge := range []string{ansi.Strip(box[0]), ansi.Strip(box[len(box)-1])} {
			if strings.ContainsAny(edge, "abcdefghijklmnopqrstuvwxyz") {
				t.Errorf("%v: text lingers on the border: %q", keys, edge)
			}
		}
		for i, line := range box[1 : len(box)-1] {
			text := inside(line)
			if !strings.HasPrefix(text, "  ") || !strings.HasSuffix(text, " ") {
				t.Errorf("%v line %d sticks to the border: %q", keys, i, text)
			}
		}
		if first, last := inside(box[1]), inside(box[len(box)-2]); strings.TrimSpace(first) != "" || strings.TrimSpace(last) != "" {
			t.Errorf("%v: the modal must breathe at the top and bottom (%q / %q)", keys, first, last)
		}
	}
}

// LOOK=<dir> drops there each TUI screen as a terminal receives it, sequences included, as
// tui-<screen>.ans: a way to read its states back without a terminal, like the panel's PNGs. cat
// renders them.
func TestLookLeavesTheTUIAsText(t *testing.T) {
	into := os.Getenv("LOOK")
	if into == "" {
		t.Skip("LOOK=<dir> drops the screens as .ans")
	}
	base := demo(t)
	shown := variants(t, base, 96, 20)
	for _, narrow := range variants(t, base, 58, 14) {
		shown = append(shown, variant{"narrow-" + narrow.name, narrow.frame})
	}
	params := resize(formed(t, formBody), 96, 20)
	params.cursor = index(t, params, "api")
	shown = append(shown, variant{"params", params.View().Content})
	stale := refused(t, resize(formed(t, formBody), 96, 20))
	shown = append(shown, variant{"config-refused", stale.View().Content},
		variant{"card-config-refused", press(stale, "i").View().Content})
	grouped := resize(formed(t, groupedBody), 96, 20)
	grouped.cursor = index(t, grouped, "deployed.traces")
	shown = append(shown, variant{"params-grouped", grouped.View().Content})
	grouped.cursor = index(t, grouped, "products")
	typing := press(grouped, "enter", "backspace", "backspace", "9", "9", "9", "9", "9")
	shown = append(shown, variant{"input", typing.View().Content},
		variant{"input-refused", press(typing, "enter").View().Content})
	if err := grouped.session.Set(map[string]string{"api": "local"}); err != nil {
		t.Fatal(err)
	}
	shown = append(shown, variant{"params-group-hidden", grouped.synced().View().Content})
	for _, one := range shown {
		if err := os.WriteFile(filepath.Join(into, "tui-"+one.name+".ans"), []byte(one.frame+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type variant struct {
	name  string
	frame string
}

// variants renders each screen right away: the session really runs and its state would change
// between building the model and displaying it.
func variants(t *testing.T, base Model, width, height int) []variant {
	sized := resize(base, width, height)
	scripts := map[string]struct {
		item string
		keys []string
	}{
		"group":               {"beta", nil},
		"app-ready":           {"front", []string{"right"}},
		"app-fallen":          {"admin", []string{"right"}},
		"app-never-started":   {"storybook", []string{"right"}},
		"facade":              {"localhost", []string{"right"}},
		"facade-subdomain":    {"admin.localhost", []string{"right"}},
		"help":                {"session", []string{"?"}},
		"no-list":             {"session", []string{"b"}},
		"filter":              {"front", []string{"right", "/"}},
		"console":             {"front", []string{"right", "enter"}},
		"card-app":            {"front", []string{"i"}},
		"card-app-log":        {"front", []string{"right", "i"}},
		"card-facade":         {"localhost", []string{"i"}},
		"card-session":        {"session", []string{"i"}},
		"card-group":          {"beta", []string{"i"}},
		"confirmation":        {"session", []string{"q"}},
		"confirmation-cancel": {"session", []string{"q", "right"}},
	}
	names := slices.Sorted(maps.Keys(scripts))
	out := make([]variant, 0, len(scripts))
	for _, name := range names {
		script := scripts[name]
		out = append(out, variant{name, press(at(t, sized, script.item), script.keys...).View().Content})
	}
	return out
}

// boxOf returns the lines of the open floating panel: the card, the help or the confirmation.
func boxOf(t *testing.T, m Model) []string {
	t.Helper()
	floats := m.floats()
	if len(floats) == 0 {
		t.Fatal("no floating panel open")
	}
	return floats[0].lines
}

// inside returns the content of a panel line, vertical borders removed.
func inside(line string) string {
	runes := []rune(ansi.Strip(line))
	if len(runes) < 2 {
		return ""
	}
	return string(runes[1 : len(runes)-1])
}

// at sets the cursor and the log on the named item: a test talks about « front », not about the
// list's third line.
func at(t *testing.T, m Model, name string) Model {
	t.Helper()
	for i, it := range m.items {
		if it.name == name {
			m.cursor, m.selected = i, i
			return m
		}
	}
	t.Fatalf("no item %q in the list", name)
	return m
}

// index returns the named item's rank, for tests that move the cursor without changing the display.
func index(t *testing.T, m Model, name string) int {
	t.Helper()
	for i, it := range m.items {
		if it.name == name {
			return i
		}
	}
	t.Fatalf("no item %q in the list", name)
	return 0
}

// press goes through Update, the program's real entry point: that is where the state settles before
// a key is read.
func press(m Model, keys ...string) Model {
	for _, key := range keys {
		next, _ := m.Update(tea.KeyPressMsg{Code: code(key), Text: text(key)})
		m = next.(Model)
	}
	return m
}

func code(key string) rune {
	switch key {
	case "tab":
		return tea.KeyTab
	case "backspace":
		return tea.KeyBackspace
	case "home":
		return tea.KeyHome
	case "end":
		return tea.KeyEnd
	case "delete":
		return tea.KeyDelete
	case "pgup":
		return tea.KeyPgUp
	case "pgdown":
		return tea.KeyPgDown
	case "up":
		return tea.KeyUp
	case "down":
		return tea.KeyDown
	case "left":
		return tea.KeyLeft
	case "right":
		return tea.KeyRight
	case "esc":
		return tea.KeyEscape
	case "enter":
		return tea.KeyEnter
	case "space":
		return tea.KeySpace
	}
	return rune(key[0])
}

func text(key string) string {
	if len(key) == 1 {
		return key
	}
	return ""
}

func resize(m Model, width, height int) Model {
	next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return next.(Model)
}

// listTop returns the first line written in the list: above it is its frame's border.
func listTop(m Model) string {
	return strings.Split(ansi.Strip(m.View().Content), "\n")[1]
}

// demo sets up a real session with three apps: one running, one that crashed, one stopped. The
// first one's port is held by a listener, which is enough for the probe to declare it ready.
func demo(t *testing.T) Model { return demoNet(t, false) }

// demoNet sets up the session, isolated or not: that is what tells the private network from the
// loopback fallback.
func demoNet(t *testing.T, isolated bool) Model {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(server.Close)
	held := core.Endpoint{IP: "127.0.0.1", Port: server.Listener.Addr().(*net.TCPAddr).Port}

	apps := []config.App{
		{Name: "front", Command: []string{"sh", "-c", "sleep 300 & wait"}, Env: map[string]string{"PORT": "{port}", "FORCE_COLOR": "1"}, Dir: t.TempDir(), Ansi: 6},
		{Name: "admin", Command: []string{"sh", "-c", "exit 2"}, Dir: t.TempDir(), Ansi: 5},
		{Name: "storybook", Command: []string{"true"}, Dir: t.TempDir(), Ansi: 2},
	}
	cfg := config.Config{
		Port: 4300,
		Dir:  t.TempDir(),
		Ingresses: []config.Ingress{
			config.NewIngress(config.Ingress{Host: "localhost", Routes: []config.Route{
				{Path: "/shop-beta/", App: "front"},
				{Path: "/api", Proxy: config.Target{URL: "https://staging.example.com", ChangeOrigin: true}},
				{Path: "/", Redirect: "/shop-beta/"},
			}}),
			config.NewIngress(config.Ingress{Host: "admin.localhost", Routes: []config.Route{
				{Path: "/admin/", App: "admin"},
				{Path: "/api/v1", Proxy: config.Target{URL: "https://admin.staging.example.com", ChangeOrigin: true}},
				{Path: "/", Redirect: "/admin/"},
			}}),
		},
		Apps: apps,
		// A group names the apps it starts, and nothing else: a facade does not start.
		Groups: []config.Group{{Name: "beta", Members: []string{"front", "storybook"}}},
	}
	session := core.NewSession(config.Form{}, cfg, heldNet{held, isolated}, ownPort{}, func(*core.Session) core.Gateway { return upGateway{} })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		session.Run(ctx, []string{"front", "admin"})
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	front, admin := session.App("front"), session.App("admin")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if front.Snapshot().Status == core.Ready && admin.Snapshot().Last != nil {
			break
		}
		if time.Now().After(deadline) {
			// Without this, a test failing further on does not say the session was not ready — and
			// without the logs, there would be no knowing why.
			t.Fatalf("session not ready: front %s, admin %+v\nfront:\n%s\nadmin:\n%s",
				front.Snapshot().Status, admin.Snapshot().Last, joinLines(front.Log.Lines()), joinLines(admin.Log.Lines()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	front.Log.Add("Application bundle generation complete. [8.067 seconds]")
	front.Log.Add("\x1b[32mComponent update sent to client(s).\x1b[0m")
	admin.Log.Add("X [ERROR] TS2339: Property 'customerId' does not exist on type 'Order'.")
	session.Facades()[0].Log.Add("GET  /admin/main.js                            200  admin      4ms")

	return newModel(session, cancel, false)
}

type upGateway struct{}

func (upGateway) Start(context.Context) error { return nil }
func (upGateway) Stop(context.Context) error  { return nil }
func (upGateway) Stats(string) (int, int)     { return 127, 1 }

// The cursor's background must not swallow the state's colour.
func TestCursorKeepsTheStatusColour(t *testing.T) {
	t.Parallel()
	m := resize(demo(t), 120, 30)

	// The background cursorOn sets, without its text colour: each segment must set it again,
	// otherwise the highlight has holes.
	background := strings.Split(sequences(lipgloss.NewStyle().Background(cursorOn.GetBackground()).Render(" ")), "|")[0]

	// The cursor lands on items in different states: their lines must differ in more than their
	// text.
	styled := map[string]string{}
	for _, name := range []string{"front", "admin"} {
		at := at(t, m, name)
		row := at.listRow(at.cursor, at.sidebarWidth(), true)
		if ansi.Strip(row) == row {
			t.Fatalf("%s: the cursor's line is not styled at all", name)
		}
		for _, code := range strings.Split(sequences(row), "|") {
			if code != "" && !strings.Contains(code, background) {
				t.Errorf("%s: a segment loses the cursor's background (%q in %s)", name, code, sequences(row))
			}
		}
		styled[name] = sequences(row)
		t.Logf("%-8s off cursor %-10s under cursor %s", name,
			sequences(colour(stateOf(at)).Render("●")), styled[name])
	}
	if styled["front"] == styled["admin"] {
		t.Errorf("both lines hold the same colours (%s): the state is lost under the cursor",
			styled["front"])
	}
}

// A background says only one thing: the keyboard is here. No inert line carries one — not even in
// its padding, where a band of colour would trail to the end of the list.
func TestOnlyTheCursorRowCarriesABackground(t *testing.T) {
	t.Parallel()
	m := resize(demo(t), 120, 30)
	background := strings.Split(sequences(lipgloss.NewStyle().Background(cursorOn.GetBackground()).Render(" ")), "|")[0]

	// The cursor tints only its own line, whatever the item.
	for name, want := range map[string]int{"front": 1, "localhost": 1, "admin.localhost": 1} {
		m.cursor = index(t, m, name)
		rows, of := m.listRows(m.sidebarWidth(), true)
		first := slices.Index(of, m.cursor)
		tinted := make([]int, 0, len(rows))
		for i, line := range rows {
			if strings.Contains(line, background) {
				tinted = append(tinted, i)
			}
		}
		if len(tinted) != want || (len(tinted) > 0 && tinted[0] != first) {
			t.Errorf("cursor on %q: tinted lines %v, want %d starting at %d\n%s",
				name, tinted, want, first, ansi.Strip(strings.Join(rows, "\n")))
		}
		// And nothing there stays dark grey, which would blend into the background.
		grey := strings.Split(sequences(dim.Render(" ")), "|")[0]
		for _, i := range tinted {
			if strings.Contains(rows[i], grey) {
				t.Errorf("cursor on %q: line %d keeps dark grey on the background (%s)",
					name, i, sequences(rows[i]))
			}
		}
	}
}

func stateOf(m Model) int {
	_, state := m.statusOf(m.items[m.cursor])
	return state
}

// sequences returns a string's style codes, its text removed.
func sequences(s string) string {
	var out []string
	for _, part := range strings.Split(s, "\x1b[") {
		if end := strings.IndexByte(part, 'm'); end >= 0 {
			out = append(out, part[:end])
		}
	}
	return strings.Join(out, "|")
}

// The console: enter the log, then into it, and what is typed goes to the app.
func TestConsoleSendsKeystrokes(t *testing.T) {
	t.Parallel()
	m := press(at(t, resize(demo(t), 120, 30), "front"), "right") // front, the running one, log focused
	if m.input != noInput {
		t.Fatal("entering the log should not open the console")
	}

	m = press(m, "enter")
	if m.input != consoleInput {
		t.Fatal("⏎ in the log should open the console")
	}
	if footer := ansi.Strip(strings.Split(m.View().Content, "\n")[m.height-1]); !strings.Contains(footer, "console") {
		t.Errorf("the footer should say typing goes to the app: %q", footer)
	}

	// Keys go to the app, echo included: what is typed shows in its log.
	for _, key := range []string{"e", "c", "h", "o", "space", "h", "e", "l", "l", "o", "enter"} {
		m = press(m, key)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(joinLines(m.session.Apps()[0].Log.Lines()), "echo hello") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := joinLines(m.session.Apps()[0].Log.Lines()); !strings.Contains(got, "echo hello") {
		t.Errorf("the app did not receive the keystrokes: %q", got)
	}

	// esc gives the keyboard back to the interface, without leaving the log.
	if m = press(m, "esc"); m.input != noInput || m.focus != onLog {
		t.Errorf("esc should leave the console: input=%v focus=%v", m.input, m.focus)
	}

	// No typing into a merged log, nor into a stopped app's.
	for _, name := range []string{"beta", "storybook"} {
		if quiet := press(at(t, m, name), "right", "enter"); quiet.input == consoleInput {
			t.Errorf("%s: the console opened on something other than a running app", name)
		}
	}
}

// q means « quit » everywhere: an open panel does not take its meaning away, esc is what closes.
func TestQuitAsksFromAnyPanel(t *testing.T) {
	t.Parallel()
	base := resize(demo(t), 120, 30)
	for name, script := range map[string][]string{
		"a facade's card": {"i"},
		"an app's card":   {"down", "i"},
		"help":            {"?"},
	} {
		m := press(at(t, base, "localhost"), script...)
		if m.modal == noModal {
			t.Fatalf("%s: the panel did not open", name)
		}
		if asked := press(m, "q"); asked.modal != confirmModal {
			t.Errorf("%s: q should ask to quit, modal=%v", name, asked.modal)
		}
	}
	// And in the confirmation, it cancels like esc.
	if closed := press(base, "q", "q"); closed.modal != noModal {
		t.Errorf("q on the question should cancel it, modal=%v", closed.modal)
	}
}

// A console only exists for the duration of a run: when the run ends, the keyboard comes back to
// the interface without a keystroke getting lost in it.
func TestConsoleClosesWithItsRun(t *testing.T) {
	t.Parallel()
	m := press(at(t, resize(demo(t), 120, 30), "front"), "right", "enter")
	if m.input != consoleInput {
		t.Fatal("⏎ on a running app should open its console")
	}

	m.session.App("front").Stop()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && m.session.App("front").Snapshot().Status != core.Idle {
		time.Sleep(20 * time.Millisecond)
	}

	// The first key after the stop acts on the interface, it does not get lost in the dead
	// terminal.
	after := press(m, "i")
	if after.input != noInput {
		t.Error("the console should have closed with its run")
	}
	if after.modal != infoModal {
		t.Errorf("the next keystroke should open the card, modal=%v", after.modal)
	}
	// And it is not reopened on a stopped app.
	if again := press(after, "esc", "enter"); again.input == consoleInput {
		t.Error("⏎ should not open a console on a stopped app")
	}
}

// Driving a facade means driving what it serves: it has no state of its own.
func TestFacadeDrivesItsApps(t *testing.T) {
	t.Parallel()
	m := at(t, resize(demo(t), 120, 30), "admin.localhost")

	press(m, "x")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && m.session.App("admin").Snapshot().Status != core.Idle {
		time.Sleep(20 * time.Millisecond)
	}
	if got := m.session.App("admin").Snapshot().Status; got != core.Idle {
		t.Errorf("x on a facade should stop the apps it serves, admin is %s", got)
	}
	if got := m.session.App("front").Snapshot().Status; got != core.Ready {
		t.Errorf("it should touch only its own, front is %s", got)
	}
}

// A group is a handle on its members: it starts the apps it names and merges their logs. The
// session is not driven.
func TestGroupDrivesItsMembers(t *testing.T) {
	t.Parallel()
	m := at(t, resize(demo(t), 120, 30), "beta")

	press(m, "s") // beta = front and storybook; never admin
	deadline := time.Now().Add(10 * time.Second)
	// storybook exits right away: its outcome, not its status, says it ran.
	for time.Now().Before(deadline) && m.session.App("storybook").Snapshot().Last == nil {
		time.Sleep(20 * time.Millisecond)
	}
	if m.session.App("storybook").Snapshot().Last == nil {
		t.Error("s on a group should start the app it names")
	}
	if got := m.session.App("admin").Snapshot().Restarts; got != 0 {
		t.Errorf("it should touch only its members, admin restarted %d times", got)
	}

	// Its log is its members'.
	merged := joinLines(m.lines())
	if !strings.Contains(merged, "Application bundle generation complete") {
		t.Errorf("the group's log should hold front's:\n%s", merged)
	}
	if strings.Contains(merged, "TS2339") {
		t.Errorf("it should not hold admin's, which is not a member:\n%s", merged)
	}

	// The session holds the port on its line, and nothing starts it.
	session := at(t, resize(demo(t), 120, 30), "session")
	if row := ansi.Strip(session.listRow(session.cursor, session.sidebarWidth(), true)); !strings.Contains(row, "port 4300") {
		t.Errorf("the session's line should hold the port: %q", row)
	}
	press(session, "s")
	time.Sleep(200 * time.Millisecond)
	if got := session.session.App("storybook").Snapshot(); got.Last != nil {
		t.Errorf("s on the session should start nothing, storybook ran: %+v", got.Last)
	}
}

// The size of the terminal lent to the apps is exactly the room their lines have: a line logWidth
// wide must display as is, neither cut nor wrapped.
func TestResizeFollowsThePanel(t *testing.T) {
	t.Parallel()
	m := demo(t)
	m2, cmd := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if cmd == nil {
		t.Fatal("a size change should be passed on to the apps")
	}
	cmd()
	m = at(t, m2.(Model), "front")

	full := strings.Repeat("x", m.logWidth())
	m.session.Apps()[0].Log.Add(full)
	if frame := ansi.Strip(m.View().Content); !strings.Contains(frame, full) {
		t.Errorf("a %d-column line does not fit in the panel:\n%s", m.logWidth(), frame)
	}

	// Closing the list gives its room to the log: the apps must learn it too.
	next, cmd := m.press(tea.KeyPressMsg{Code: 'b', Text: "b"})
	if cmd == nil {
		t.Fatal("opening or closing the list should be passed on to the apps")
	}
	if wider := next.(Model); wider.logWidth() != m.logWidth()+m.sidebarWidth() {
		t.Errorf("without the list, the log is %d columns instead of %d",
			wider.logWidth(), m.logWidth()+m.sidebarWidth())
	}
}

// A line longer than the panel wraps: it reads in full, and scrolling counts the screen lines it
// takes.
func TestLongLinesFold(t *testing.T) {
	t.Parallel()
	m := at(t, press(resize(demo(t), 96, 24), "b"), "front") // without the list: the log takes the whole screen
	// No space or punctuation: what is read back must be the line, character for character.
	long := strings.Repeat("abcdefghij", 30)
	m.session.Apps()[0].Log.Add(long)

	if frame := ansi.Strip(m.View().Content); strings.Contains(frame, "…") {
		t.Errorf("the line was cut instead of wrapped:\n%s", frame)
	}
	if got := strings.Join(logRows(m), ""); !strings.Contains(got, long) {
		t.Errorf("the wrapped line does not read back in full:\n%s", got)
	}
	rows := (len(long) + m.logWidth() - 1) / m.logWidth()
	if m.logLength() < rows {
		t.Errorf("%d screen lines while a %d-column line takes %d",
			m.logLength(), len(long), rows)
	}

	// Scrolling goes up to the first screen line and no further: the panel always shows something.
	// That takes a log taller than the panel.
	for i := range 40 {
		m.session.Apps()[0].Log.Addf("line %d", i)
	}
	small := resize(m, 96, 14)
	for _, keys := range [][]string{{"g"}, {"pgup", "pgup", "pgup", "pgup", "pgup", "pgup", "pgup"}} {
		if first := logRows(press(small, keys...))[0]; !strings.HasPrefix(first, "$ ") {
			t.Errorf("%v: the top of the log should show the launched command, shows %q", keys, first)
		}
	}
}

// fold cuts at the column and leaves each chunk closed on its styles.
func TestFoldKeepsStylesInsideTheirRow(t *testing.T) {
	t.Parallel()
	if got := fold("short", 10); len(got) != 1 || got[0] != "short" {
		t.Errorf("a line that fits must not be touched: %q", got)
	}
	if got := fold(strings.Repeat("x", 10), 10); len(got) != 1 {
		t.Errorf("a line exactly at the width fits on one screen line: %q", got)
	}
	rows := fold("\x1b[36m"+strings.Repeat("ab", 12)+"\x1b[0m", 10)
	if len(rows) != 3 {
		t.Fatalf("24 columns in 10 should make 3 lines: %q", rows)
	}
	for i, row := range rows {
		if got := ansi.StringWidth(row); got > 10 {
			t.Errorf("line %d is %d wide", i, got)
		}
		if ansi.Strip(row) == row {
			t.Errorf("line %d lost its colour: %q", i, row)
		}
		if !strings.HasSuffix(row, "\x1b[0m") {
			t.Errorf("line %d leaves its style open: %q", i, row)
		}
	}
	if got := ansi.Strip(strings.Join(rows, "")); got != strings.Repeat("ab", 12) {
		t.Errorf("the wrapped text changed: %q", got)
	}
}

// logRows returns the log's lines as they read: the border, the name line and the blank after it
// removed. The list must be closed, otherwise the line holds both columns.
func logRows(m Model) []string {
	lines := strings.Split(m.View().Content, "\n")
	rows := make([]string, 0, m.logHeight())
	for _, line := range lines[3 : 3+m.logHeight()] {
		rows = append(rows, strings.TrimSpace(inside(line)))
	}
	return rows
}

func joinLines(lines []core.Line) string {
	var out []string
	for _, line := range lines {
		out = append(out, ansi.Strip(line.Text))
	}
	return strings.Join(out, "\n")
}

// In the console, arrows go to the app as from a real terminal; what it does with them is its own
// business. Only page scrolling stays with the interface.
func TestConsoleSendsArrowsAndKeepsPaging(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"right": "\x1b[C", "left": "\x1b[D", "up": "\x1b[A", "down": "\x1b[B",
		"home": "\x1b[H", "end": "\x1b[F", "delete": "\x1b[3~", "enter": "\r", "backspace": "\x7f",
		"tab": "\t", "space": " ", "a": "a",
	} {
		if got := keystroke(tea.KeyPressMsg{Code: code(name), Text: text(name)}); got != want {
			t.Errorf("%q passed on as %q, want %q", name, got, want)
		}
	}
	// ctrl+c too: that is what lets the program be interrupted from its log.
	if got := keystroke(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); got != "\x03" {
		t.Errorf("ctrl+c passed on as %q, want \\x03", got)
	}

	m := at(t, resize(demo(t), 120, 30), "front")
	for i := range 100 {
		m.session.Apps()[0].Log.Addf("line %d", i)
	}
	m = press(m, "right", "enter")
	if m.input != consoleInput {
		t.Fatal("the console should be open")
	}
	// An arrow is not intercepted by the interface: it goes to the app.
	if after := press(m, "right", "left", "up"); after.offset != m.offset || after.input != consoleInput {
		t.Errorf("the arrows acted on the interface: offset=%d input=%v", after.offset, after.input)
	}
	if after := press(m, "pgup"); after.offset == m.offset {
		t.Error("pgup should scroll the log")
	}
}

// heldNet gives the apps the address of a listener held by the test: the probe finds it open, so «
// front » reaches Ready without a real server starting.
type heldNet struct {
	ep       core.Endpoint
	isolated bool
}

func (h heldNet) Assign(string) (core.Endpoint, error) { return h.ep, nil }

func (h heldNet) Dial(ctx context.Context, ep core.Endpoint) (net.Conn, error) {
	return core.Loopback{}.Dial(ctx, ep)
}

// ownPort takes the port itself: the view shows nothing of the hub.
type ownPort struct{}

func (ownPort) Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", port))
}

func (ownPort) Hosts([]string) error { return nil }

func (h heldNet) Mode() core.Mode {
	if h.isolated {
		return core.Mode{Name: "netns", Detail: "10.77.0.0/24", Isolated: true}
	}
	return core.Mode{Name: "loopback", Detail: "127.0.0.1, ephemeral ports", Reason: "for the test"}
}

// When the apps are not isolated, a badge says so at all times, without having to navigate, and the
// session's card gives the reason. On a private network, there is nothing to report.
func TestFallbackIsAlwaysVisible(t *testing.T) {
	t.Parallel()
	fallback := resize(demo(t), 120, 30)

	footer := ansi.Strip(strings.Split(fallback.View().Content, "\n")[29])
	if !strings.Contains(footer, "⚠ loopback") {
		t.Errorf("the badge should always be visible: %q", footer)
	}
	// It does not eat the shortcuts, and the line is still full width.
	if !strings.Contains(footer, "s/x/r") {
		t.Errorf("the shortcuts vanished under the badge: %q", footer)
	}
	if got := ansi.StringWidth(footer); got != 120 {
		t.Errorf("the bottom line is %d columns, want 120", got)
	}
	reason := ansi.Strip(strings.Join(boxOf(t, press(fallback, "i")), "\n"))
	if !strings.Contains(reason, "fallback: for the test") {
		t.Errorf("the session's card should say why: %q", reason)
	}

	isolated := resize(demoNet(t, true), 120, 30)
	if got := ansi.Strip(strings.Split(isolated.View().Content, "\n")[29]); strings.Contains(got, "⚠") {
		t.Errorf("on a private network, nothing to report: %q", got)
	}
}

// A gap between the process and the declaration shows in the list, without opening anything: a
// marker at the end of the line, and the matching word as soon as the app is watched.
func TestDriftShowsInTheList(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	held := core.Endpoint{IP: "127.0.0.1", Port: server.Listener.Addr().(*net.TCPAddr).Port}

	dir := t.TempDir()
	path := filepath.Join(dir, config.JSONName)
	declare := func(mark string) {
		t.Helper()
		body := fmt.Sprintf(`{
  "port": 4300,
  "ingress": [{ "host": "localhost", "routes": [{ "path": "/", "app": "app" }] }],
  "apps": [{ "name": "app", "command": ["sh", "-c", "sleep 300 & wait", %q, "{port}"] }]
}`, mark)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declare("first")
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := core.NewSession(form, *cfg, heldNet{held, false}, ownPort{}, func(*core.Session) core.Gateway { return upGateway{} })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		session.Run(ctx, []string{"app"})
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	app := session.App("app")
	deadline := time.Now().Add(20 * time.Second)
	for app.Snapshot().Status != core.Ready {
		if time.Now().After(deadline) {
			t.Fatalf("the app is not ready: %s\n%s", app.Snapshot().Status, joinLines(app.Log.Lines()))
		}
		time.Sleep(20 * time.Millisecond)
	}

	m := resize(newModel(session, cancel, false), 120, 30)
	if frame := ansi.Strip(m.View().Content); strings.Contains(frame, "↻") {
		t.Errorf("nothing changed, the list should hold no marker:\n%s", frame)
	}

	declare("second")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	m = at(t, m.synced(), "app")
	frame := ansi.Strip(m.View().Content)
	if !strings.Contains(frame, "↻") {
		t.Errorf("the list should mark the app to restart:\n%s", frame)
	}
	if !strings.Contains(frame, "restart pending") {
		t.Errorf("the watched app should say what it awaits:\n%s", frame)
	}
}

// One wheel notch is one step, in the panel it hovers — the mouse does not need focus.
func TestWheelMovesOneStep(t *testing.T) {
	t.Parallel()
	m := resize(demo(t), 120, 30)
	if m.View().MouseMode == tea.MouseModeNone {
		t.Fatal("without mouse tracking, the terminal turns the wheel into multiplied arrows")
	}

	m = wheel(m, tea.MouseWheelDown, 2)
	if m.cursor != 1 || m.offset != 0 {
		t.Fatalf("one notch on the list: cursor=%d offset=%d, want 1 and 0", m.cursor, m.offset)
	}
	if m = wheel(m, tea.MouseWheelUp, 2); m.cursor != 0 {
		t.Fatalf("one notch back: cursor=%d, want 0", m.cursor)
	}

	// The log moves up one line — without the list's cursor moving. It needs something to scroll:
	// on a log shorter than its panel, there is nothing above to bring in.
	front := m.session.App("front")
	for i := 0; i < 100; i++ {
		front.Log.Addf("line %d", i)
	}
	m = press(m, "down", "down", "space")
	journal := m.sidebarWidth() + 4
	cursor := m.cursor
	m = wheel(m, tea.MouseWheelUp, journal)
	if m.offset != 1 || m.cursor != cursor {
		t.Fatalf("one notch on the log: offset=%d cursor=%d, want 1 and %d", m.offset, m.cursor, cursor)
	}
	if m = wheel(m, tea.MouseWheelDown, journal); m.offset != 0 {
		t.Fatalf("one notch down: offset=%d, want 0", m.offset)
	}

	// A modal takes the screen: the wheel moves nothing behind it.
	card := press(m, "i")
	if got := wheel(card, tea.MouseWheelDown, 2); got.cursor != card.cursor {
		t.Fatalf("with the card open, the wheel moved the cursor: %d → %d", card.cursor, got.cursor)
	}
}

func wheel(m Model, button tea.MouseButton, x int) Model {
	next, _ := m.Update(tea.MouseWheelMsg{X: x, Y: 3, Button: button})
	return next.(Model)
}

// The help says what q really does, and that depends on the terminal: on one's own, leaving stops
// everything; on a lent terminal, the default is to leave the session behind.
func TestTheHelpSaysWhereQLeads(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		lent   bool
		wanted string
	}{
		{false, "asks how to leave, on “Quit”"},
		{true, "asks how to leave, on “Leave the TUI”"},
	} {
		m := press(resize(newModel(demo(t).session, func() {}, c.lent), 120, 40), "?")
		frame := ansi.Strip(m.View().Content)
		if !strings.Contains(frame, c.wanted) {
			t.Errorf("lent terminal = %v: the help for q does not say %q:\n%s", c.lent, c.wanted, frame)
		}
	}
}

// The mouse chooses, it does not act: a click in the list shows the item it touches, a click in the
// log gives it focus, and what holds no item moves nothing.
func TestClickChoosesWhatItPointsAt(t *testing.T) {
	t.Parallel()
	m := resize(demo(t), 120, 30)

	m = click(m, 4, screenRow(t, m, "storybook"))
	if got := m.items[m.cursor].name; got != "storybook" || m.selected != m.cursor {
		t.Fatalf("click on storybook: cursor on %q, selected %d", got, m.selected)
	}
	if m.focus != onList {
		t.Error("a click in the list should not leave the list")
	}

	// A section title, a rule and a route are not items: the cursor has nothing to do there.
	for _, text := range []string{"apps", "/api", "staging.example.com"} {
		if after := click(m, 4, screenRow(t, m, text)); after.cursor != m.cursor || after.selected != m.selected {
			t.Errorf("the click on %q moved the cursor: %d → %d", text, m.cursor, after.cursor)
		}
	}

	// The log takes focus without changing what it displays, nor what the cursor designates.
	shown, cursor := m.selected, m.cursor
	m = click(m, m.width-10, logTop+1)
	if m.focus != onLog || m.selected != shown || m.cursor != cursor {
		t.Fatalf("click in the log: focus=%v selected=%d cursor=%d, want the log, %d and %d",
			m.focus, m.selected, m.cursor, shown, cursor)
	}

	// The footer holds nothing: it does not give focus to the log.
	back := press(m, "esc")
	if after := click(back, 4, back.height-1); after.focus != onList {
		t.Errorf("a click on the footer gave focus to the log")
	}

	// A modal takes the screen: neither the click nor the drag gets behind it.
	opened := press(m, "i")
	if after := click(opened, 4, screenRow(t, m, "session")); after.cursor != opened.cursor || after.modal != infoModal {
		t.Errorf("with the card open, the click acted behind it: cursor %d → %d", opened.cursor, after.cursor)
	}
	if after := motion(click(opened, 4, 4), 20, 6); !after.sel.empty() {
		t.Error("with the card open, the drag selected behind it")
	}
}

// When narrow, the list and the log do not fit together: choosing an item means entering it, as
// with space.
func TestClickEntersTheLogOnANarrowScreen(t *testing.T) {
	t.Parallel()
	m := resize(demo(t), 60, 20)
	m = click(m, 4, screenRow(t, m, "storybook"))
	if m.items[m.selected].name != "storybook" || m.focus != onLog {
		t.Fatalf("click when narrow: shown %q focus=%v, want storybook and the log",
			m.items[m.selected].name, m.focus)
	}
}

// Dragging in the log copies what was covered: the log's lines joined by a newline, without style
// and without the trailing blank.
func TestDragCopiesWhatIsShown(t *testing.T) {
	t.Parallel()
	m := at(t, press(resize(demo(t), 96, 24), "b"), "front") // without the list, the log takes the whole screen
	app := m.session.Apps()[0]
	app.Log.Add("first line")
	app.Log.Add("second line")

	from := screenRow(t, m, "first line")
	left := column(screenLine(m, from), "first")
	m, cmd := drag(m, left, from, left+len("second line")-1, from+1)
	if cmd == nil {
		t.Fatal("the release should put the text on the clipboard")
	}
	if got := m.copied(); got != "first line\nsecond line" {
		t.Errorf("copied %q, want both lines joined by a newline", got)
	}
	if !strings.Contains(m.View().Content, reverseOn) {
		t.Error("the selection does not show on screen")
	}

	// A click that does not drag selects nothing: it only gave focus.
	if quiet := click(m, left, from); !quiet.sel.empty() || quiet.copied() != "" {
		t.Errorf("a lone click selected %q", quiet.copied())
	}

	// A line wrapped over two screen lines stays one line: copying puts no newline in it.
	long := strings.Repeat("abcdefghij", 12)
	app.Log.Add(long)
	tail := len(long) - m.logWidth()
	start := screenRow(t, m, long[:20])
	m, _ = drag(m, left, start, left+tail-1, start+1)
	if got := m.copied(); got != long {
		t.Errorf("a wrapped line was copied as %q", got)
	}

	// A keystroke takes over: what the mouse held does not survive it.
	if typed := press(m, "G"); !typed.sel.empty() || strings.Contains(typed.View().Content, reverseOn) {
		t.Error("a keystroke should clear the selection")
	}
}

// The selection lives in log coordinates: it does not move when the screen scrolls, it is simply
// drawn where its lines went.
func TestSelectionFollowsTheScroll(t *testing.T) {
	t.Parallel()
	m := at(t, press(resize(demo(t), 96, 24), "b"), "front")
	app := m.session.Apps()[0]
	for i := range 60 {
		app.Log.Addf("line %d", i)
	}

	from := screenRow(t, m, "line 59")
	left := column(screenLine(m, from), "line")
	m, _ = drag(m, left, from, left+len("line 59")-1, from)
	if got := m.copied(); got != "line 59" {
		t.Fatalf("copied %q, want the clicked line", got)
	}
	painted := reverseRows(m)
	if len(painted) != 1 || painted[0] != from {
		t.Fatalf("the selection paints on %v, want only line %d", painted, from)
	}

	// One wheel notch: the log moves up one line, the selection with it, and its text is the same.
	scrolled := wheel(m, tea.MouseWheelUp, 4)
	if got := scrolled.copied(); got != "line 59" {
		t.Errorf("after scrolling, the selection holds %q", got)
	}
	if painted := reverseRows(scrolled); len(painted) != 1 || painted[0] != from+1 {
		t.Errorf("after one notch, the selection paints on %v, want line %d", painted, from+1)
	}
}

// Two clicks on the same cell take the word under it: the terminal does not say « double », the
// delay does.
func TestDoubleClickTakesTheWord(t *testing.T) {
	t.Parallel()
	m := at(t, press(resize(demo(t), 96, 24), "b"), "front")
	m.session.Apps()[0].Log.Add("a hello here")

	row := screenRow(t, m, "a hello here")
	at := column(screenLine(m, row), "hello")
	if got := click(click(m, at, row), at, row).copied(); got != "hello" {
		t.Errorf("two clicks on the word took %q", got)
	}
	// The same gesture, but too slow: the second click only sets the anchor again.
	slow := click(m, at, row)
	slow.sel.at = time.Now().Add(-time.Second)
	if got := click(slow, at, row).copied(); got != "" {
		t.Errorf("two spaced-out clicks took %q", got)
	}
	// On a blank, there is no word to take.
	space := column(screenLine(m, row), " hello")
	if got := click(click(m, space, row), space, row).copied(); got != "" {
		t.Errorf("two clicks on a blank took %q", got)
	}
}

// reverseOn is the start of the sequence lipgloss writes for reverse video: that is how a taken
// cell is recognized.
var reverseOn = "\x1b[" + strings.Split(sequences(picked.Render(" ")), "|")[0] + "m"

// reverseRows returns the screen lines where something is in reverse video.
func reverseRows(m Model) []int {
	var out []int
	for i, line := range strings.Split(m.View().Content, "\n") {
		if strings.Contains(line, reverseOn) {
			out = append(out, i)
		}
	}
	return out
}

// screenRow returns the screen line where this text reads: a test clicks where it would be read.
func screenRow(t *testing.T, m Model, text string) int {
	t.Helper()
	for i, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
		if strings.Contains(line, text) {
			return i
		}
	}
	t.Fatalf("nothing holds %q on screen:\n%s", text, ansi.Strip(m.View().Content))
	return 0
}

// column returns the screen column where this text starts: the frame's borders weigh more than one
// byte, and a click lands on a cell.
func column(line, text string) int {
	return ansi.StringWidth(line[:strings.Index(line, text)])
}

func screenLine(m Model, y int) string {
	return strings.Split(ansi.Strip(m.View().Content), "\n")[y]
}

func click(m Model, x, y int) Model {
	next, _ := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	return next.(Model)
}

// motion is a movement with the button held: mouse tracking only reports those, and it is the drag
// itself.
func motion(m Model, x, y int) Model {
	next, _ := m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
	return next.(Model)
}

// drag makes the whole gesture — click, drag, release — and returns the command that sets the copy.
func drag(m Model, fromX, fromY, toX, toY int) (Model, tea.Cmd) {
	m = motion(click(m, fromX, fromY), toX, toY)
	next, cmd := m.Update(tea.MouseReleaseMsg{X: toX, Y: toY, Button: tea.MouseLeft})
	return next.(Model), cmd
}

// A parameter is an item under the session: the name on the left, its value on the right, and ⏎ or
// space moves it to the next choice without touching what the log displays.
// formBody declares a choice, two flags one of which is hidden, a disabled flag and a text.
const formBody = `import { config, disabled, form, hidden } from "dsvs";
const params = form({ api: ["local", "staging"], slowApi: false, fakePayments: false, traces: false, branch: "main" }, (p) => {
  hidden(p.fakePayments);
  disabled(p.traces, { when: "only from the company network" });
});
export default config(params, ({ api, slowApi }) => ({
  port: 4300,
  ingress: [{ host: "localhost", routes: [{ path: "/", app: "front" }] }],
  apps: [{ name: "front", command: ["sleep", "300", api, String(slowApi), "{port}"] }],
}));`

// formed sets up a real session on this configuration, which node evaluates: the list holds its
// form.
func formed(t *testing.T, body string) Model {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	held := core.Endpoint{IP: "127.0.0.1", Port: server.Listener.Addr().(*net.TCPAddr).Port}

	path := filepath.Join(t.TempDir(), "config.dsvs.ts")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	form, cfg, err := config.Load(path, nil, config.Input{})
	if err != nil {
		t.Fatal(err)
	}
	session := core.NewSession(form, *cfg, heldNet{held, false}, ownPort{}, func(*core.Session) core.Gateway { return upGateway{} })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		session.Run(ctx, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	return newModel(session, cancel, false)
}

// A configuration the session does not apply shows on its line, and its card says why; the
// evaluation that applies it again clears both.
func TestARefusedConfigurationShowsOnTheSession(t *testing.T) {
	t.Parallel()
	m := refused(t, resize(formed(t, formBody), 120, 30))
	if row := ansi.Strip(m.listRow(0, m.sidebarWidth(), true)); !strings.Contains(row, "✗ port 4300") {
		t.Errorf("the session's line says nothing: %q", row)
	}
	card := ansi.Strip(strings.Join(boxOf(t, press(m, "i")), "\n"))
	if !strings.Contains(card, "not applied") || !strings.Contains(card, "SyntaxError") {
		t.Errorf("the session's card does not say why:\n%s", card)
	}

	if err := os.WriteFile(m.session.Config().Path, []byte(formBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.session.Reload(); err != nil {
		t.Fatal(err)
	}
	if row := ansi.Strip(m.listRow(0, m.sidebarWidth(), true)); strings.Contains(row, "✗") {
		t.Errorf("applied again, the session still says so: %q", row)
	}
}

// refused breaks this session's configuration and makes it reread it: the session keeps its own,
// and says so.
func refused(t *testing.T, m Model) Model {
	t.Helper()
	if err := os.WriteFile(m.session.Config().Path, []byte(formBody+","), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.session.Reload(); err == nil {
		t.Fatal("an unreadable configuration reloads")
	}
	return m
}

func TestParametersAreTurnedFromTheList(t *testing.T) {
	t.Parallel()
	m := resize(formed(t, formBody), 120, 30)
	session := m.session
	frame := ansi.Strip(m.View().Content)
	for _, want := range []string{"api", "local", "slowApi", "○", "branch", "main"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the list does not show %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "fakePayments") {
		t.Errorf("the list shows a hidden field:\n%s", frame)
	}
	if names := []string{m.items[1].name, m.items[2].name, m.items[3].name, m.items[4].name}; !slices.Equal(names, []string{"api", "slowApi", "traces", "branch"}) {
		t.Fatalf("the parameters should follow the form, the list is %v", m.items)
	}
	value := func(name string) string {
		field, _ := session.Form().Field(name)
		return field.Value
	}

	m.cursor = 1
	m = press(m, "enter")
	if got := value("api"); got != "staging" {
		t.Errorf("api = %q after ⏎, want staging", got)
	}
	m = m.synced()
	m.cursor = 2
	m = press(m, "space")
	if got := value("slowApi"); got != "true" {
		t.Errorf("slowApi = %q after space, want true", got)
	}
	generation := session.Generation()
	m.cursor = 3
	m = press(m, "enter", "space")
	m.cursor = 4
	m = press(m, "space")
	if session.Generation() != generation || value("traces") != "false" || value("branch") != "main" {
		t.Error("a disabled field or a text moved with a list gesture")
	}
	m = m.synced()
	if frame := ansi.Strip(m.View().Content); !strings.Contains(frame, "●") {
		t.Errorf("the list does not show the raised flag:\n%s", frame)
	}
	if m.items[m.selected].kind != kindSession {
		t.Errorf("the log shows %q, it should stay on the session", m.items[m.selected].name)
	}
}

// groupedBody declares a group hidden when api=local — without the build reading api: hiding the
// group only changes the form —, a disabled flag with its reason, a bounded number and a text a
// form rule refuses.
const groupedBody = `import { config, disabled, form, hidden, max, min, validateTree } from "dsvs";
const params = form({ api: ["staging", "local"], deployed: { version: ["v2", "v3"], traces: false }, products: 20, branch: "main" }, (p) => {
  hidden(p.deployed, { when: ({ valueOf }) => valueOf(p.api) === "local" });
  disabled(p.deployed.traces, { when: "traces are only reachable from the company network" });
  min(p.products, 0);
  max(p.products, 5000);
  validateTree(p, ({ valueOf }) => (valueOf(p.branch) === "wip" ? "wip is not a branch" : null));
});
export default config(params, ({ products, branch }) => ({
  port: 4300,
  ingress: [{ host: "localhost", routes: [{ path: "/", app: "front" }] }],
  apps: [{ name: "front", command: ["sleep", "300", String(products), branch, "{port}"] }],
}));`

// A group is written under its title, its fields indented under it, and goes with its title when
// everything it contains is hidden — even if the configuration does not change.
func TestParametersComeInGroups(t *testing.T) {
	t.Parallel()
	m := resize(formed(t, groupedBody), 120, 30)
	list := func() []string {
		rows, _ := m.listRows(m.listWidth(m.listPanelWidth()), false)
		for i, line := range rows {
			rows[i] = strings.TrimRight(ansi.Strip(line), " ")
		}
		return rows
	}
	rows := list()
	title := slices.Index(rows, indent(childIndent)+"deployed")
	if title < 0 || !strings.HasPrefix(rows[title+1], indent(childIndent+2)+"version") || !strings.HasPrefix(rows[title+2], indent(childIndent+2)+"traces") {
		t.Fatalf("the deployed group is not written under its title:\n%s", strings.Join(rows, "\n"))
	}

	generation := m.session.Generation()
	if err := m.session.Set(map[string]string{"api": "local"}); err != nil {
		t.Fatal(err)
	}
	m = m.synced()
	if m.session.Generation() != generation {
		t.Fatal("hiding the group changed the configuration: the test no longer sees what it looks for")
	}
	if rows := list(); slices.ContainsFunc(rows, func(line string) bool {
		return strings.Contains(line, "deployed") || strings.Contains(line, "traces")
	}) {
		t.Errorf("the hidden group stays in the list:\n%s", strings.Join(rows, "\n"))
	}
}

// Under the cursor, a parameter states at the top of the log what it has to say: a disabled field's
// reasons, the refusal of the last gesture made on it. Elsewhere, the log is whole.
func TestTheLogOpensOnWhatTheParameterSays(t *testing.T) {
	t.Parallel()
	m := resize(formed(t, groupedBody), 120, 30)
	head := func() string { return ansi.Strip(strings.Join(m.logPanel(false), "\n")) }

	m.cursor = index(t, m, "deployed.traces")
	if said := head(); !strings.Contains(said, "disabled: traces are only reachable from the company network") {
		t.Errorf("a disabled flag does not say why:\n%s", said)
	}
	m.cursor = index(t, m, "api")
	if said := head(); strings.Contains(said, "disabled") || strings.Contains(said, "✗") {
		t.Errorf("a parameter with nothing to say writes:\n%s", said)
	}

	m.cursor = index(t, m, "branch")
	m = press(m, "enter", "backspace", "backspace", "backspace", "backspace", "w", "i", "p", "enter")
	if said := head(); !strings.Contains(said, "✗ the form refuses it: wip is not a branch") {
		t.Errorf("the input's refusal does not read:\n%s", said)
	}
	if field, _ := m.session.Form().Field("branch"); field.Value != "main" {
		t.Errorf("a refused gesture changed branch to %q", field.Value)
	}
	m = press(m, "enter", "enter")
	if said := head(); strings.Contains(said, "✗") {
		t.Errorf("the refusal survives the next gesture:\n%s", said)
	}
}

// A chosen value the file no longer accepts yields to its bound, and the TUI says so while it
// lasts: an arrow on its line, the sentence at the top of the log.
func TestAValueThatNoLongerHoldsSaysSo(t *testing.T) {
	t.Parallel()
	m := resize(formed(t, groupedBody), 120, 30)
	if err := m.session.Set(map[string]string{"products": "4000"}); err != nil {
		t.Fatal(err)
	}
	path := m.session.Config().Path
	if err := os.WriteFile(path, []byte(strings.Replace(groupedBody, "max(p.products, 5000)", "max(p.products, 1000)", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.session.Reload(); err != nil {
		t.Fatal(err)
	}
	m.cursor = index(t, m, "products")
	if row := ansi.Strip(m.listRow(m.cursor, m.sidebarWidth(), true)); !strings.Contains(row, "↓ 1000") {
		t.Errorf("the line does not say that products yields: %q", row)
	}
	if said := ansi.Strip(strings.Join(m.logPanel(false), "\n")); !strings.Contains(said, "↓ products=4000 no longer holds (at most 1000): products=1000 until it does") {
		t.Errorf("the log does not open on what yields:\n%s", said)
	}
}

// A configuration that uses a hidden field gets it said on the session's card.
func TestTheSessionCardSaysWhatTheFormNotices(t *testing.T) {
	t.Parallel()
	body := strings.Replace(groupedBody, `branch, "{port}"] }]`, `branch, "{port}"], env: { V: String(deployed.version) } }]`, 1)
	body = strings.Replace(body, "({ products, branch })", "({ products, branch, deployed })", 1)
	m := resize(formed(t, body), 120, 30)
	if err := m.session.Set(map[string]string{"api": "local"}); err != nil {
		t.Fatal(err)
	}
	if card := ansi.Strip(strings.Join(m.sessionCard(), "\n")); !strings.Contains(card, "deployed.version, deployed.traces are hidden, yet apps.front.env.V depends on them") {
		t.Errorf("the card does not say the configuration uses a hidden field:\n%s", card)
	}
}

// ⏎ on a text or a number opens an input in place of its value: what is typed is written at the
// cursor, ⏎ sends it to the session, and esc drops it without sending anything.
func TestATextOrANumberIsTypedFromTheList(t *testing.T) {
	t.Parallel()
	m := resize(formed(t, groupedBody), 120, 30)
	value := func(name string) string {
		field, _ := m.session.Form().Field(name)
		return field.Value
	}
	m.cursor = index(t, m, "products")
	m = press(m, "enter")
	if row := ansi.Strip(m.listRow(m.cursor, m.sidebarWidth(), true)); !strings.Contains(row, "[20 ]") {
		t.Errorf("the input does not open on the value: %q", row)
	}
	m = press(m, "backspace", "backspace", "3", "0", "0", "enter")
	if got := value("products"); got != "300" || m.input != noInput {
		t.Errorf("products = %q after typing 300, input open: %v", got, m.input == paramInput)
	}

	generation := m.session.Generation()
	m = press(m, "enter", "home", "9", "esc")
	if m.session.Generation() != generation || value("products") != "300" {
		t.Errorf("esc sent the input: products = %q", value("products"))
	}
	m = press(m, "enter", "home", "1", "enter")
	if got := value("products"); got != "1300" {
		t.Errorf("a keystroke at the start gives %q, want 1300", got)
	}
}
