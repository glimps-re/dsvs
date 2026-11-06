package tui

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"dsvs/internal/build"
	"dsvs/internal/core"
)

// narrowWidth is the width below which the list and the log no longer fit side by side: focus then
// designates which of the two takes the screen.
const narrowWidth = 80

type focus int

const (
	onList focus = iota
	onLog
)

// Where typing goes. The filter, the console and a parameter's input cannot be open at the same
// time.
type input int

const (
	noInput input = iota
	filterInput
	consoleInput
	paramInput
)

// One modal at a time: two floating panels cannot be open together.
type modal int

const (
	noModal modal = iota
	infoModal
	helpModal
	confirmModal
	quittingModal
)

// cancelButton is the confirmation's first button: the actions come after it, on the right, as in
// the system's dialogs, and the one that has focus on opening is the last.
const cancelButton = 0

// quit is a way to leave. all says that the session stops with the TUI; otherwise the TUI hands
// back and the session goes on — a choice that only exists when someone else holds it.
type quit struct {
	label string
	all   bool
}

type kind int

const (
	// The session holds the port and the network plumbing. It is not driven: it is there as long as
	// the tool runs.
	kindSession kind = iota
	// A parameter is set, not watched: the log stays on what it was showing.
	kindParam
	// A group is declared in the configuration: it starts its members and merges their logs.
	kindGroup
	kindApp
	// A facade opens its section: it is its title, and its routes are written below.
	kindIngress
)

// item is a line of the list where the cursor stops. group holds the title of the section it opens,
// when it opens one; a facade does not need it, its name is the title. Its routes are not items:
// their log would be the facade's, and the cursor has nothing to gain there.
type item struct {
	kind  kind
	name  string
	group string
	app   *core.App
	ing   *core.Ingress
}

type Model struct {
	session *core.Session
	stop    func()
	// view is the window this interface announces to the session: what it lends the apps. events is
	// its subscription to changes; unwatch closes it when the interface goes away.
	view    *core.Viewport
	events  <-chan core.Event
	unwatch func()

	// gen is the configuration generation the list is laid out on: as soon as the session announces
	// another one, the list is rebuilt. The generation is authoritative, not the events — the
	// channel can overflow and lose a wake-up, never the generation.
	gen uint64

	items    []item
	cursor   int
	selected int
	focus    focus
	sidebar  bool

	modal  modal
	button int
	// quits are the ways to leave the confirmation offers, the last one having focus. asked is what
	// reads above them.
	quits  []quit
	asked  string
	input  input
	filter string
	// draft is a parameter's input: it only holds under paramInput, and nothing goes out before ⏎.
	draft draft
	// refusal is the refusal of the last gesture on a parameter: it is written at the top of the
	// log as long as the cursor is on that parameter, and the next gesture clears it.
	refusal refusal
	offset  int
	// sel is what the mouse took in the log. Its zero value says no anchor is set: there is then
	// nothing to extend, nothing to paint and nothing to copy.
	sel selection

	width, height int
}

// draft is a text being written for the parameter it names — a click that moves the list's cursor
// does not divert it —, and the cursor's position in that text.
type draft struct {
	name string
	text []rune
	at   int
}

// refusal is what a session answered to a gesture on a parameter, when it refused it.
type refusal struct{ name, why string }

// spot is a point of the log: the line, among those lines returns, and the column in its unwrapped
// text. A selection lives there rather than on the screen, to follow scrolling and so that a line
// wrapped over three screen lines stays one line.
type spot struct{ line, col int }

// selection is what the mouse holds in the log: from is the anchor set on click, to the end the
// drag moves, at the anchor's instant — whose absence says there is no anchor. With both points at
// the same place, nothing is selected: the click only gave focus.
type selection struct {
	from, to spot
	at       time.Time
}

func (s selection) empty() bool    { return s.from == s.to }
func (s selection) anchored() bool { return !s.at.IsZero() }

// span puts the selection back in reading order; both ends are taken.
func (s selection) span() (first, last spot) {
	if s.to.line < s.from.line || (s.to.line == s.from.line && s.to.col < s.from.col) {
		return s.to, s.from
	}
	return s.from, s.to
}

// doubleClick is the delay under which two clicks on the same cell count as one: the terminal does
// not say « double », it only reports clicks.
const doubleClick = 400 * time.Millisecond

// Terminal is where the TUI is displayed. Empty, it is the process's own: the TUI and the session
// then live together, and quitting the TUI closes the session. Filled, it comes from a client that
// lent it — the session outlives it, so it can be quit without closing the session.
type Terminal struct {
	In, Out *os.File
	Cols    int
	Rows    int
	Env     []string
}

// Program is a running TUI. Resize tells it its window has changed: that is what whoever holds it
// for an attached client needs.
type Program struct {
	program *tea.Program
	view    *core.Viewport
	unwatch func()
}

// New sets the TUI on a terminal; stop triggers stopping the apps, whose end the interface awaits
// before giving the terminal back.
func New(ctx context.Context, session *core.Session, stop func(), on Terminal) *Program {
	options := []tea.ProgramOption{tea.WithContext(ctx)}
	lent := on.In != nil
	if lent {
		// The process's signals are not this TUI's: the terminal is elsewhere, its size arrives
		// through Resize, and a SIGTERM belongs to the session. Without WithoutSignalHandler,
		// bubbletea's handler stays stuck sending a message to a finished loop and its shutdown
		// never returns (measured: the session survived its SIGTERM).
		options = append(options, tea.WithInput(on.In), tea.WithOutput(on.Out),
			tea.WithWindowSize(on.Cols, on.Rows), tea.WithEnvironment(on.Env), tea.WithoutSignalHandler())
	}
	model := newModel(session, stop, lent)
	return &Program{program: tea.NewProgram(model, options...), view: model.view, unwatch: model.unwatch}
}

func (p *Program) Run() error {
	// The window and the subscription are given back on leaving: the apps must not keep this
	// window's size, and the session has no interface to wake that is no longer there.
	defer p.view.Close()
	defer p.unwatch()
	_, err := p.program.Run()
	return err
}

// Resize gives the TUI the size of the lent terminal: it does not come from a signal, since that
// terminal is not this process's.
func (p *Program) Resize(cols, rows int) {
	p.program.Send(tea.WindowSizeMsg{Width: cols, Height: rows})
}

func newModel(session *core.Session, stop func(), lent bool) Model {
	quits, asked := []quit{{label: "Quit", all: true}}, "stop the apps and quit?"
	if lent {
		quits = []quit{{label: "Close everything", all: true}, {label: "Leave the TUI"}}
		asked = "the apps run without the TUI."
	}
	events, unwatch := session.Watch()
	return Model{session: session, stop: stop, view: session.Viewport(), events: events, unwatch: unwatch,
		sidebar: true, items: itemsOf(session), gen: session.Generation(), quits: quits, asked: asked}
}

// itemsOf lays the list out on the configuration in force. A reload rebuilds it: groups and facades
// come in as they are declared at that instant.
func itemsOf(session *core.Session) []item {
	// The session opens the list: it holds the port, and the log of what belongs to no facade. No
	// section title above it, it is the only one of its kind.
	items := []item{{kind: kindSession, name: session.Log.Name()}}
	// A parameter opens its group's title when the group changes: a group whose fields are all
	// hidden therefore has none.
	group := ""
	for _, field := range session.Form().Fields {
		if field.Hidden {
			continue
		}
		row := item{kind: kindParam, name: field.Name}
		if within, _ := field.Group(); within != group {
			group, row.group = within, within
		}
		items = append(items, row)
	}
	for i, group := range session.Config().Groups {
		set := item{kind: kindGroup, name: group.Name}
		if i == 0 {
			set.group = "groups"
		}
		items = append(items, set)
	}
	for i, app := range session.Apps() {
		// An app does not say who serves it: the link is circumstantial, and it reads under the
		// facades.
		row := item{kind: kindApp, name: app.Name, app: app}
		if i == 0 {
			row.group = "apps"
		}
		items = append(items, row)
	}
	for _, in := range session.Facades() {
		items = append(items, item{kind: kindIngress, name: in.Cfg.Name, ing: in})
	}
	return items
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(waitEvent(m.events), tick())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// The console only exists for the duration of a run: when the run ends, the keyboard comes back
	// to the interface by itself. Without that, the console stayed open on a vanished terminal,
	// where every key got lost.
	if m.input == consoleInput {
		if shown := m.items[m.selected]; shown.kind != kindApp || shown.app.Snapshot().Status != core.Ready {
			m.input = noInput
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// The apps format their output to the size of the terminal they are lent: it may as well be
		// that of the panel where they are read.
		return m, lendSize(m.view, m.logWidth(), m.logHeight())

	case eventMsg:
		if m.modal == quittingModal && m.allIdle() {
			return m, tea.Quit
		}
		return m.synced(), waitEvent(m.events)

	case tickMsg:
		if m.modal == quittingModal && m.allIdle() {
			return m, tea.Quit
		}
		return m.synced(), tick()

	case tea.KeyPressMsg:
		// Typing replaces what the mouse held: the selection survives no keystroke.
		m.sel = selection{}
		return m.press(msg)

	case tea.MouseWheelMsg:
		return m.wheel(msg), nil

	case tea.MouseClickMsg:
		return m.click(msg), nil

	case tea.MouseMotionMsg:
		return m.drag(msg), nil

	case tea.MouseReleaseMsg:
		return m.release(msg)
	}
	return m, nil
}

func (m Model) press(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	name := key.String()
	if m.modal != noModal {
		return m.pressInModal(name)
	}
	switch m.input {
	case filterInput:
		return m.editFilter(key), nil
	case consoleInput:
		return m.typeInto(key)
	case paramInput:
		return m.editParam(key), nil
	}

	switch name {
	case "q", "ctrl+c":
		m.modal, m.button = confirmModal, m.byDefault()

	case "up", "k":
		if m.focus == onList {
			m.cursor = clamp(m.cursor-1, 0, len(m.items)-1)
		} else {
			m.offset = m.scrolled(m.offset + 1)
		}

	case "down", "j":
		if m.focus == onList {
			m.cursor = clamp(m.cursor+1, 0, len(m.items)-1)
		} else {
			m.offset = m.scrolled(m.offset - 1)
		}

	case "right", "l", "enter":
		// Three steps: choose what to watch, enter it, then write into it. When narrow there is
		// nothing to watch until it is entered, so the first two happen at once.
		switch {
		case m.target().kind == kindParam && name == "enter":
			m = m.enterParam()
		case m.focus == onLog && name == "enter":
			m.attach()
		case m.selectCursor() || m.narrow():
			m.focus = onLog
		}

	case "space", " ":
		if m.target().kind == kindParam {
			m = m.turn()
		} else {
			m.selectCursor()
			if m.narrow() {
				m.focus = onLog
			}
		}

	case "left", "h", "esc", "escape":
		m.focus, m.sidebar = onList, true

	case "i":
		// The card is driven by the cursor: align it on the target before opening.
		m.showCard(m.targetIndex())

	case "?":
		m.modal = helpModal

	case "b":
		m.sidebar = !m.sidebar
		m.focus = onLog
		if m.sidebar {
			m.focus = onList
		}
		// The list takes its place from the log: the apps format to what remains.
		return m, lendSize(m.view, m.logWidth(), m.logHeight())

	case "s":
		m.apply((*core.App).Start, m.appsOf)

	case "x":
		m.apply((*core.App).Stop, m.released)

	case "r":
		m.apply((*core.App).Restart, m.appsOf)

	case "g":
		m.offset = m.scrolled(m.logLength())

	case "G":
		m.offset = 0

	case "pgup":
		m.offset = m.scrolled(m.offset + m.logHeight())

	case "pgdown":
		m.offset = m.scrolled(m.offset - m.logHeight())

	case "/":
		m.input, m.focus = filterInput, onLog

	case "c":
		m.filter = ""

	case "o":
		return m, open(m.url())

	case "!":
		return m, shell(m.session)

	case "R":
		// The file is already reread on every save: this key is there to force it, and to catch up
		// on what lives outside its folder.
		_ = m.session.Reload()

	case "u":
		// The session switches to the binary on disk without letting go of its apps: this interface
		// falls with the exec, and the client that lent it reattaches on the other side. A refusal
		// reads in the session's log.
		_ = m.session.Renew()
	}
	return m, nil
}

// wheel scrolls what the mouse hovers by one notch, regardless of focus: the list moves one item,
// the log one line. That is why the view requests mouse tracking — without it, the terminal turns
// the wheel into arrows multiplied by the system setting, and one notch skipped several items. An
// open modal does not scroll.
func (m Model) wheel(msg tea.MouseWheelMsg) Model {
	step := 0
	switch msg.Button {
	case tea.MouseWheelUp:
		step = 1
	case tea.MouseWheelDown:
		step = -1
	default:
		return m
	}
	if m.modal != noModal {
		return m
	}
	if m.overList(msg.X) {
		m.cursor = clamp(m.cursor-step, 0, len(m.items)-1)
		return m
	}
	m.offset = m.scrolled(m.offset + step)
	return m
}

// click chooses what to watch, or sets a selection's anchor, depending on the panel clicked.
// Actions stay on the keyboard: a click neither starts nor stops anything. A modal lets nothing
// through behind it, as for the wheel.
func (m Model) click(msg tea.MouseClickMsg) Model {
	if m.modal != noModal || msg.Button != tea.MouseLeft {
		return m
	}
	// A click always takes back what the mouse held; the previous one still serves to recognize a
	// double.
	previous := m.sel
	m.sel = selection{}
	// The footer holds neither an item nor a log line.
	if msg.Y >= m.height-1 {
		return m
	}
	if m.overList(msg.X) {
		if at := m.itemAt(msg.Y); at >= 0 {
			m.cursor = at
			m.selectCursor()
			// When narrow, the list and the log do not fit together: choosing means entering.
			if m.narrow() {
				m.focus = onLog
			}
		}
		return m
	}
	m.focus = onLog
	at, ok := m.spotAt(msg.X, msg.Y)
	if !ok {
		return m
	}
	m.sel = selection{from: at, to: at, at: time.Now()}
	// Two clicks: the previous one was at the same place, recent, and had taken nothing — a drag
	// clicked again is not a double click, and the word already taken is not taken again.
	if previous.empty() && previous.anchored() && at == previous.from && time.Since(previous.at) < doubleClick {
		m.sel.from, m.sel.to = m.wordAt(at)
	}
	return m
}

// drag extends the selection. Mouse tracking only reports motion with a button held: this message
// is the drag itself, and nothing else needs to say a drag is going on.
func (m Model) drag(msg tea.MouseMotionMsg) Model {
	if m.modal != noModal || msg.Button != tea.MouseLeft || !m.sel.anchored() {
		return m
	}
	if at, ok := m.spotAt(msg.X, msg.Y); ok {
		m.sel.to = at
	}
	return m
}

// release closes the selection by copying it. An empty selection does not exist: a click that does
// not drag only gave focus.
func (m Model) release(msg tea.MouseReleaseMsg) (tea.Model, tea.Cmd) {
	if m.modal != noModal || msg.Button != tea.MouseLeft || m.sel.empty() {
		return m, nil
	}
	return m, copyText(m.copied(), m.session.Log)
}

// synced lays the list out on the configuration in force when its generation has moved, or when the
// form shows other fields without it moving — a field the network hides —, and touches nothing
// otherwise.
func (m Model) synced() Model {
	if gen := m.session.Generation(); gen != m.gen || !sameRows(itemsOf(m.session), m.items) {
		m.gen = gen
		return m.relist()
	}
	return m
}

// sameRows says that two lists hold the same rows, in the same order.
func sameRows(a, b []item) bool {
	return slices.EqualFunc(a, b, func(x, y item) bool { return x.kind == y.kind && x.name == y.name && x.group == y.group })
}

// relist lays the list out on the configuration just reloaded. The cursor and the displayed item
// are found again by name: an added facade does not move what was being watched, and a removed
// facade brings back to the session. This is the only place that rebuilds the list — whether the
// reload comes from a save or from the key.
func (m Model) relist() Model {
	cursor, shown := m.items[m.cursor], m.items[m.selected]
	// The log no longer returns the same lines: what the mouse held no longer exists.
	m.sel = selection{}
	m.items = itemsOf(m.session)
	m.cursor, m.selected = indexOf(m.items, cursor), indexOf(m.items, shown)
	return m
}

// indexOf returns this item's rank, a parameter possibly bearing an app's name, or the session's,
// which is always there.
func indexOf(items []item, wanted item) int {
	for i, it := range items {
		if it.kind == wanted.kind && it.name == wanted.name {
			return i
		}
	}
	return 0
}

// turn moves the parameter under the cursor to its next choice. The session replays its
// configuration; a refusal is written at the top of the log. An inert field, or one with no choices
// to cycle, does not move.
func (m Model) turn() Model {
	field, _ := m.session.Form().Field(m.target().name)
	if turns := field.Choices != nil && !field.Disabled && !field.Readonly; turns {
		return m.set(field.Name, field.Next())
	}
	return m
}

// enterParam is ⏎ on a parameter: a text or a number opens for input on its value, the rest moves
// to the next choice.
func (m Model) enterParam() Model {
	field, _ := m.session.Form().Field(m.target().name)
	if field.Choices != nil || field.Disabled || field.Readonly {
		return m.turn()
	}
	m.input, m.draft = paramInput, draft{name: field.Name, text: []rune(field.Value), at: len([]rune(field.Value))}
	return m
}

// set gives a parameter a value, as one fills in a form: a choice that requires another one waits
// for the field that completes it (core.Session.Draft). It keeps the refusal if there is one.
func (m Model) set(name, value string) Model {
	m.refusal = refusal{}
	if err := m.session.Draft(map[string]string{name: value}); err != nil {
		m.refusal = refusal{name, err.Error()}
	}
	return m
}

// editParam carries a keystroke to a parameter's input: ⏎ sends what was written, esc drops it, and
// the rest is written at the cursor or moves it.
func (m Model) editParam(key tea.KeyPressMsg) Model {
	d := m.draft
	text := slices.Clone(d.text)
	switch key.String() {
	case "enter":
		m.input = noInput
		return m.set(d.name, string(text))
	case "esc", "escape":
		m.input = noInput
	case "left":
		d.at = max(0, d.at-1)
	case "right":
		d.at = min(len(text), d.at+1)
	case "home", "ctrl+a":
		d.at = 0
	case "end", "ctrl+e":
		d.at = len(text)
	case "backspace":
		if d.at > 0 {
			text, d.at = slices.Delete(text, d.at-1, d.at), d.at-1
		}
	case "delete":
		if d.at < len(text) {
			text = slices.Delete(text, d.at, d.at+1)
		}
	default:
		if typed := []rune(key.Key().Text); len(typed) > 0 {
			text, d.at = slices.Insert(text, d.at, typed...), d.at+len(typed)
		}
	}
	d.text = text
	m.draft = d
	return m
}

// attach gives the keyboard to what the log displays: the console of a running app. A facade has
// nothing to receive — its routing table is read, it is not edited here.
func (m *Model) attach() {
	switch target := m.items[m.selected]; target.kind {
	case kindApp:
		if target.app.Snapshot().Status != core.Ready {
			target.app.Log.Add("-- nothing is running: no one to read what we type")
			return
		}
		m.input, m.offset = consoleInput, 0
	}
}

// showCard opens an item's card for reading.
func (m *Model) showCard(index int) { m.cursor, m.modal = index, infoModal }

// typeInto sends the keystroke to the app, as a terminal would have passed it on — arrows included:
// what the app does with it is its own business. Only esc and page scrolling stay with the
// interface.
func (m Model) typeInto(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch name := key.String(); name {
	case "esc", "escape":
		m.input = noInput
		return m, nil
	case "pgup":
		m.offset = m.scrolled(m.offset + m.logHeight())
		return m, nil
	case "pgdown":
		m.offset = m.scrolled(m.offset - m.logHeight())
		return m, nil
	}
	if bytes := keystroke(key); bytes != "" {
		if err := m.items[m.selected].app.Send(bytes); err != nil {
			m.items[m.selected].app.Log.Addf("!! %v", err)
			m.input = noInput
		}
	}
	return m, nil
}

// keystroke translates a keystroke into what a terminal would have passed on.
func keystroke(key tea.KeyPressMsg) string {
	switch name := key.String(); name {
	case "enter":
		return "\r"
	case "backspace":
		return "\x7f"
	case "tab":
		return "\t"
	case "space":
		return " "
	case "up":
		return "\x1b[A"
	case "down":
		return "\x1b[B"
	case "right":
		return "\x1b[C"
	case "left":
		return "\x1b[D"
	case "home":
		return "\x1b[H"
	case "end":
		return "\x1b[F"
	case "delete":
		return "\x1b[3~"
	default:
		// ctrl+letter: the terminal passes on the matching control code, ctrl+c included.
		if rest, ok := strings.CutPrefix(name, "ctrl+"); ok && len(rest) == 1 && rest[0] >= 'a' && rest[0] <= 'z' {
			return string(rune(rest[0] & 0x1f))
		}
		return key.Key().Text
	}
}

func (m Model) editFilter(key tea.KeyPressMsg) Model {
	switch key.String() {
	case "enter":
		m.input = noInput
	case "esc", "escape":
		m.input, m.filter = noInput, ""
	case "backspace":
		if m.filter != "" {
			m.filter = m.filter[:len(m.filter)-1]
		}
	default:
		if text := key.Key().Text; text != "" {
			m.filter += text
		}
	}
	m.offset = 0
	return m
}

// selectCursor makes the item under the cursor the one the log displays, and says whether it
// already was. A parameter has no log: it is never the displayed item, and that is what spares the
// rest of the view from mentioning it.
func (m *Model) selectCursor() (already bool) {
	if m.selected == m.cursor || m.items[m.cursor].kind == kindParam {
		return true
	}
	m.selected, m.offset = m.cursor, 0
	return false
}

// pressInModal: the modal has the keyboard and lets nothing through behind it.
func (m Model) pressInModal(name string) (tea.Model, tea.Cmd) {
	// q and ctrl+c mean « quit » everywhere, panel open or not: closing is esc. Without that, each
	// q only unfolded one layer and the key seemed dead.
	if name == "q" || name == "ctrl+c" {
		switch m.modal {
		case quittingModal:
			return m, tea.Quit // the shutdown drags on: hand back right away
		case confirmModal:
			m.modal = noModal // the question is already asked: q cancels it, like esc
			return m, nil
		default:
			m.modal, m.button = confirmModal, m.byDefault()
			return m, nil
		}
	}

	switch m.modal {

	case confirmModal:
		switch name {
		case "left", "h", "up", "k":
			// Three buttons: an arrow must go its own way, otherwise it cycles at random.
			m.button = (m.button + len(m.quits)) % (len(m.quits) + 1)
		case "right", "l", "down", "j", "tab":
			m.button = (m.button + 1) % (len(m.quits) + 1)
		case "enter", "space", " ":
			return m.choose(m.button)
		case "o", "y":
			return m.choose(m.byDefault())
		case "n", "esc", "escape":
			m.modal = noModal
		}

	case infoModal:
		// The cursor stays alive under the card: cards scroll without closing it.
		switch name {
		case "up", "k":
			m.showCard(clamp(m.cursor-1, 0, len(m.items)-1))
		case "down", "j":
			m.showCard(clamp(m.cursor+1, 0, len(m.items)-1))
		case "esc", "escape", "i":
			m.modal = noModal
		}

	case helpModal:
		switch name {
		case "esc", "escape", "?":
			m.modal = noModal
		}
	}
	return m, nil
}

// byDefault is the button that has focus on opening: the last one, so leaving without closing
// anything whenever possible.
func (m Model) byDefault() int { return len(m.quits) }

// choose applies a confirmation button. Quitting without closing everything hands back right away:
// there is nothing to wait for, the session goes on without us.
func (m Model) choose(button int) (tea.Model, tea.Cmd) {
	if button == cancelButton {
		m.modal = noModal
		return m, nil
	}
	if !m.quits[button-1].all {
		return m, tea.Quit
	}
	m.modal = quittingModal
	m.stop()
	return m, tick()
}

func (m Model) targetIndex() int {
	if m.focus == onList {
		return m.cursor
	}
	return m.selected
}

// target is what the focused panel designates: the item under the cursor in the list, the displayed
// one when in the log. Actions, like the info panel, apply to it.
func (m Model) target() item {
	return m.items[m.targetIndex()]
}

// apply runs a command on the target: on a group or a facade, the apps resolve draws from it; on an
// app, that app alone. A refusal is written to the session's log, which the TUI shows.
func (m Model) apply(do func(*core.App) error, resolve func(name string) []*core.App) {
	steer := func(app *core.App) {
		if err := do(app); err != nil {
			m.session.Log.Addf("!! %v", err)
		}
	}
	switch target := m.target(); target.kind {
	case kindGroup, kindIngress:
		for _, app := range resolve(target.name) {
			steer(app)
		}
	case kindApp:
		steer(target.app)
	case kindSession:
		m.session.Log.Add("-- the session is not steered; its groups and its apps are")
	}
}

// appsOf returns the apps a name brings along: its own, a facade's, a group's.
func (m Model) appsOf(name string) []*core.App {
	names, _ := m.session.Config().AppsOf(name)
	return m.supervised(names)
}

// released returns the apps a stop of this name takes along, like dsvs stop: a facade keeps those
// another facade still serves, and the session's log says which.
func (m Model) released(name string) []*core.App {
	cfg := m.session.Config()
	names, _ := cfg.AppsOf(name)
	release := cfg.Release([]string{name}, names)
	if said := release.String(); said != "" {
		m.session.Log.Addf("-- %s", said)
	}
	return m.supervised(release.Stops)
}

// supervised returns the supervisors of these apps, those the session holds.
func (m Model) supervised(names []string) []*core.App {
	out := make([]*core.App, 0, len(names))
	for _, n := range names {
		if app := m.session.App(n); app != nil {
			out = append(out, app)
		}
	}
	return out
}

func (m Model) narrow() bool { return m.width < narrowWidth }

func (m Model) allIdle() bool {
	for _, app := range m.session.Apps() {
		if app.Snapshot().Status != core.Idle {
			return false
		}
	}
	return true
}

// lines returns the displayed log, filtered when relevant. A group merges its members' logs: two
// apps recompiling together then read in the same stream.
func (m Model) lines() []core.Line {
	var all []core.Line
	switch shown := m.items[m.selected]; shown.kind {
	case kindGroup:
		members := m.appsOf(shown.name)
		series := make([][]core.Line, 0, len(members))
		for _, app := range members {
			series = append(series, written(app.Log.Lines()))
		}
		all = core.Merge(series...)
	case kindIngress:
		all = shown.ing.Log.Lines()
	case kindApp:
		all = shown.app.Log.Lines()
	case kindSession:
		all = m.session.Log.Lines()
	}
	if m.filter == "" {
		return all
	}
	kept := all[:0:0]
	for _, line := range all {
		if strings.Contains(strings.ToLower(line.Text), strings.ToLower(m.filter)) {
			kept = append(kept, line)
		}
	}
	return kept
}

// written drops the current line when it is empty: that is where a log's cursor is, and a source
// that said nothing has nothing to say in a merged stream.
func written(lines []core.Line) []core.Line {
	if n := len(lines); n > 0 && lines[n-1].Text == "" {
		return lines[:n-1]
	}
	return lines
}

// logLength is the height of what there is to scroll: screen lines, wraps included, since that is
// what the panel shows.
func (m Model) logLength() int {
	rows, _ := m.screen(m.lines())
	return len(rows)
}

// scrolled bounds scrolling: up to the first screen line and no further, otherwise the log leaves
// through the top and the panel shows only emptiness.
func (m Model) scrolled(offset int) int {
	return clamp(offset, 0, max(0, m.logLength()-m.logHeight()))
}

// url is the address through which to reach the target: a facade's root, where its catch-all leads,
// or the first path serving one of its apps — for a group, the first one it brings along.
func (m Model) url() string {
	target := m.target()
	if target.kind == kindIngress {
		return target.ing.URL("/")
	}
	names, _ := m.session.Config().AppsOf(target.name)
	for _, name := range names {
		for _, in := range m.session.Facades() {
			// A route path has no trailing slash; an app address does.
			if paths := in.Cfg.PathsOf(name); len(paths) > 0 {
				return in.URL(strings.TrimSuffix(paths[0], "/") + "/")
			}
		}
	}
	if len(m.session.Facades()) > 0 {
		return m.session.Facades()[0].URL("/")
	}
	return ""
}

type eventMsg core.Event

type tickMsg time.Time

// waitEvent waits for the session's next wake-up. With the subscription closed, there is nothing
// left to wait for: the command returns an empty message and the loop stops there.
func waitEvent(events <-chan core.Event) tea.Cmd {
	return func() tea.Msg {
		event, live := <-events
		if !live {
			return nil
		}
		return eventMsg(event)
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// lendSize tells the session the size of the panel where logs are read. The smallest window among
// the attached interfaces decides what the apps receive: here, only ours is concerned.
func lendSize(view *core.Viewport, cols, rows int) tea.Cmd {
	return func() tea.Msg {
		view.Resize(cols, rows)
		return nil
	}
}

// shell lends the terminal to an interactive shell. The interface runs in the apps' network: this
// shell is therefore already there, with nothing to cross — a curl sees there what an app sees.
func shell(session *core.Session) tea.Cmd {
	name := os.Getenv("SHELL")
	if name == "" {
		name = "/bin/sh"
	}
	command := exec.Command(name)
	command.Env = append(os.Environ(), "PS1="+build.Name()+":"+session.Mode().Name+"$ ")
	return tea.ExecProcess(command, func(err error) tea.Msg {
		if err != nil {
			session.Log.Addf("!! %s: %v", name, err)
		}
		return nil
	})
}

func open(url string) tea.Cmd {
	return func() tea.Msg {
		if url != "" {
			_ = exec.Command("xdg-open", url).Start()
		}
		return nil
	}
}

func clamp(v, low, high int) int {
	return max(low, min(v, high))
}
