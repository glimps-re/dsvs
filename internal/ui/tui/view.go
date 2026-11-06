package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"dsvs/internal/config"
	"dsvs/internal/core"
)

const (
	// The list's columns: a section title and a facade line up, what they contain is indented, and
	// a proxy's target is indented from its path.
	starWidth   = 3
	childIndent = starWidth + 3
	valueIndent = childIndent + 2
	// The modal keeps blank space on each side of its text, borders included.
	boxMargin = 6
	// logTop is the log's first screen line: its border, the line of its name and the rule after it
	// are above, and hold no line.
	logTop = 3
)

// The ANSI colours the interface uses. States take the bright shades: the dark ones render an olive
// yellow that is almost brown in most palettes, and colours must hold on the cursor's background
// too anyway — same state, same shade everywhere.
const (
	red     = 9
	green   = 10
	yellow  = 11
	magenta = 13
	grey    = 8
)

var (
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	accent = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	strong = lipgloss.NewStyle().Bold(true)
	// A background says only one thing: this is interactive and that is where the keys are. The
	// cursor takes it tinted when its panel has the keyboard, grey otherwise; nothing inert carries
	// one.
	cursorOn  = lipgloss.NewStyle().Background(lipgloss.Color("24")).Foreground(lipgloss.Color("15"))
	cursorOff = lipgloss.NewStyle().Background(lipgloss.Color("237")).Foreground(lipgloss.Color("250"))
	// A button is filled: the primary one takes the accent, the other stays grey.
	primary   = lipgloss.NewStyle().Background(lipgloss.Color("6")).Foreground(lipgloss.Color("0")).Bold(true)
	secondary = lipgloss.NewStyle().Background(lipgloss.Color("238")).Foreground(lipgloss.Color("252"))
	// The app's cursor takes the text colour, as a real terminal would.
	ptyCursor = lipgloss.NewStyle().Reverse(true)
	// What the mouse took reads in reverse video, like a terminal's selection — it is the one being
	// replaced, mouse tracking having taken it away.
	picked = lipgloss.NewStyle().Reverse(true)
	bad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	warn   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	plain  = lipgloss.NewStyle()
)

// colours holds one style per ANSI colour; colour returns the one for any integer.
var colours = func() [16]lipgloss.Style {
	var styles [16]lipgloss.Style
	for i := range styles {
		styles[i] = lipgloss.NewStyle().Foreground(lipgloss.Color(strconv.Itoa(i)))
	}
	return styles
}()

func colour(ansi int) lipgloss.Style { return colours[ansi%len(colours)] }

// onCursor adapts a state colour to the cursor's background: only grey needs brightening there,
// where it blends with the rest of the line.
func onCursor(ansi int) int {
	if ansi == grey {
		return 7
	}
	return ansi
}

// part is a piece of a line with its style, and row assembles them in one go: the cursor's
// background must run under the whole line, and a nested style ends with a reset that would wipe it
// out.
type part struct {
	text  string
	style lipgloss.Style
}

// row returns a line at the given width; a zero width returns it as is, which serves to measure it
// before knowing the room it gets. back is set only on the cursor's line.
func row(parts []part, width int, back lipgloss.Style, on bool) string {
	var out strings.Builder
	left := width
	for _, p := range parts {
		text := p.text
		if width > 0 {
			text = truncate(text, left)
			left -= ansi.StringWidth(text)
		}
		style := p.style
		if on {
			style = style.Background(back.GetBackground())
			// Dark grey blends into the cursor's background: it brightens, like a state's grey.
			if style.GetForeground() == dim.GetForeground() {
				style = style.Foreground(lipgloss.Color("252"))
			}
		}
		out.WriteString(style.Render(text))
	}
	if left > 0 {
		// The padding takes the cursor's background with the rest of the line, and nothing at all
		// otherwise: an inert line trailing a band of colour would say the keyboard is there.
		if on {
			out.WriteString(back.Render(strings.Repeat(" ", left)))
		} else {
			out.WriteString(strings.Repeat(" ", left))
		}
	}
	return out.String()
}

// aligned pushes the right-hand pieces to the end of the line, leaving them at least two blanks.
func aligned(width int, left, right []part) []part {
	used := 0
	for _, p := range append(slices.Clone(left), right...) {
		used += ansi.StringWidth(p.text)
	}
	return append(append(left, part{strings.Repeat(" ", max(2, width-used)), plain}), right...)
}

func (m Model) View() tea.View {
	view := tea.NewView("")
	view.AltScreen = true
	// Without this mode, the terminal turns the wheel into arrows, as many as one notch is worth in
	// lines for the system: we decide the step, in wheel.
	view.MouseMode = tea.MouseModeCellMotion
	if m.width < 8 || m.height < 6 {
		return view
	}

	screen := append(m.body(), m.footer())
	for _, float := range m.floats() {
		screen = float.over(screen)
	}
	view.Content = strings.Join(screen, "\n")
	return view
}

// float is a panel laid over the screen, at the place it chose for itself.
type float struct {
	lines []string
	x, y  int
}

func (f float) over(screen []string) []string {
	out := append([]string(nil), screen...)
	for i, line := range f.lines {
		if f.y+i < 0 || f.y+i >= len(out) {
			continue
		}
		under := out[f.y+i]
		out[f.y+i] = pad(ansi.Truncate(under, f.x, ""), f.x) + line +
			ansi.TruncateLeft(under, f.x+ansi.StringWidth(line), "")
	}
	return out
}

// floats returns the current floating panels, from bottom to top.
func (m Model) floats() []float {
	switch m.modal {
	case quittingModal:
		return []float{m.box("stopping", "ctrl+c forces", []string{"stopping the apps…"})}
	case confirmModal:
		question := m.asked
		buttons := m.buttons()
		indent := max(0, (ansi.StringWidth(question)-ansi.StringWidth(buttons))/2)
		return []float{m.box("quit", "←→ · ⏎", []string{question, "", strings.Repeat(" ", indent) + buttons})}
	case helpModal:
		return []float{m.box("keys", "esc", helpLines(m.quits[m.byDefault()-1].label))}
	case infoModal:
		return []float{m.card()}
	}
	return nil
}

// card draws the item under the cursor, opened for reading.
func (m Model) card() float {
	target := m.items[m.cursor]
	return m.box(target.name, "↑↓ · esc", m.cardLines(target))
}

// cardLines describes the target in sections.
func (m Model) cardLines(target item) []string {
	switch target.kind {
	case kindIngress:
		return m.ingressCard(target.ing)
	case kindApp:
		return m.appCard(target.app)
	case kindGroup:
		return m.groupCard(target.name)
	}
	return m.sessionCard()
}

// appCard: an app says what it does and what launches it. How it is reached is none of its
// business — the facade decides that, and that is where it reads.
func (m Model) appCard(app *core.App) []string {
	snap := app.Snapshot()
	state := []string{describe(snap)}
	if snap.PID != 0 {
		state = append(state, fmt.Sprintf("pid %d · started %s ago", snap.PID, uptime(snap.Since)))
	}
	// When stopped, describe already says how the last run ended: only add since when.
	if snap.Last != nil {
		since := uptime(snap.Last.At) + " ago"
		if snap.Status == core.Idle {
			state[0] += " " + since
		} else {
			state = append(state, fmt.Sprintf("%s %s", snap.Last, since))
		}
	}
	if snap.Restarts > 0 {
		state = append(state, plural(snap.Restarts, "restart"))
	}
	if mark := drift(app); mark != "" {
		state = append(state, mark)
	}

	cfg := app.Cfg()
	launch := []string{strings.Join(cfg.Command, " ")}
	if env := env(cfg.Env); env != "" {
		launch = append(launch, env)
	}
	launch = append(launch, "in "+cfg.Dir)

	return sections(section("state", state), section("launch", launch))
}

// ingressCard: what the facade does, how it is reached, and its routing table.
func (m Model) ingressCard(in *core.Ingress) []string {
	snap := in.Snapshot()
	state := []string{
		plural(snap.Requests, "request") + " · " + plural(snap.Failures, "error"),
		"served as long as the session runs",
	}

	width := 0
	for _, route := range in.Cfg.Routes {
		width = max(width, ansi.StringWidth(route.Path))
	}
	routes := make([]string, 0, len(in.Cfg.Routes))
	for _, route := range in.Cfg.Routes {
		routes = append(routes, pad(route.Path, width)+"  "+m.leadsTo(route))
	}
	return sections(
		section("state", state),
		section("network", []string{in.URL("") + " · off route: 404"}),
		section("routes", routes))
}

// sessionCard is the full card: what the session does not apply of its configuration and why, what
// its form says without refusing anything, the network the apps live in, their state, and what each
// facade does with each path.
func (m Model) sessionCard() []string {
	var stale []string
	if why := m.session.NotApplied(); why != "" {
		// The card sizes itself on its content within the screen's width: a long reason wraps there
		// rather than being cut.
		for _, line := range strings.Split(ansi.Wrap(why, max(1, m.width-2-boxMargin), ""), "\n") {
			stale = append(stale, colour(red).Render(line))
		}
	}
	var notices []string
	for _, notice := range m.session.Form().Notices {
		for _, line := range strings.Split(ansi.Wrap(notice, max(1, m.width-2-boxMargin), ""), "\n") {
			notices = append(notices, colour(yellow).Render(line))
		}
	}
	sets := []([]string){
		section("not applied", stale),
		section("notice", notices),
		section("network", append(append([]string{"port " + strconv.Itoa(m.session.Config().Port)}, m.modeLines()...),
			"observed · "+m.session.Observed().String())),
		section("apps", appLines(m.session.Apps())),
	}
	for _, in := range m.session.Facades() {
		paths := 0
		for _, route := range in.Cfg.Routes {
			paths = max(paths, ansi.StringWidth(route.Path))
		}
		routes := make([]string, 0, len(in.Cfg.Routes)+1)
		routes = append(routes, dim.Render(in.URL("")))
		for _, route := range in.Cfg.Routes {
			routes = append(routes, pad(route.Path, paths)+"  "+m.leadsTo(route))
		}
		sets = append(sets, section(in.Cfg.Name, routes))
	}
	return sections(sets...)
}

// groupCard says what a group gathers: its apps, and where they stand.
func (m Model) groupCard(name string) []string {
	return section("apps", appLines(m.appsOf(name)))
}

// appLines names apps and says where they stand, aligned on the same column.
func appLines(apps []*core.App) []string {
	width := 0
	for _, app := range apps {
		width = max(width, ansi.StringWidth(app.Name))
	}
	out := make([]string, 0, len(apps))
	for _, app := range apps {
		out = append(out, colour(app.Cfg().Ansi).Render(pad(app.Name, width))+"  "+status(app))
	}
	return out
}

// leadsTo says where a route leads. No type glyph: an app's colour, a target's URL and a redirect's
// « to » are enough to tell them apart, and three vocabularies on one line was one too many.
func (m Model) leadsTo(route config.Route) string {
	switch route.Kind() {
	case config.ToRedirect:
		return dim.Render("to " + route.Redirect)
	case config.ToProxy:
		return route.Proxy.URL
	}
	return m.appLabel(route.App)
}

// appLabel paints an app's name in its log's colour: that is what links a route to what is read
// elsewhere.
func (m Model) appLabel(name string) string {
	for _, app := range m.session.Apps() {
		if app.Name == name {
			return colour(app.Cfg().Ansi).Render(name)
		}
	}
	return name
}

func (m Model) body() []string {
	height := m.height - 1
	// When a panel floats, it has the keyboard: the rest goes idle.
	awake := m.modal == noModal
	if m.narrow() {
		if m.focus == onList {
			return m.listColumn(m.listPanelWidth(), height, awake)
		}
		return m.logPanel(awake)
	}
	if !m.sidebar {
		return m.logPanel(awake)
	}
	left := m.listColumn(m.listPanelWidth(), height, awake && m.focus == onList)
	right := m.logPanel(awake && m.focus == onLog)
	out := make([]string, height)
	for i := range out {
		out[i] = left[i] + right[i]
	}
	return out
}

// listColumn frames the list like the log. Inside, section titles and rules — a rule gives air, it
// does not delimit, the border delimits. It scrolls when it is taller than its frame, keeping the
// cursor in the middle.
func (m Model) listColumn(width, height int, active bool) []string {
	rows, of := m.listRows(m.listWidth(width), active)
	room := height - 2 // what the frame leaves between its two borders
	from := listFrom(len(rows), room, slices.Index(of, m.cursor))
	return panel("", rows[from:min(len(rows), from+room)], width, height, active)
}

// listFrom is the first line of the list that its frame shows: it only scrolls when the list is
// taller than the frame, and it then keeps the cursor in the middle. Rendering and clicking both
// ask for it, which keeps them on the same layout.
func listFrom(rows, room, cursor int) int {
	if rows <= room {
		return 0
	}
	return clamp(cursor-room/2, 0, rows-room)
}

// listPanelWidth is the width of the list's frame: the whole screen when the list takes it alone.
func (m Model) listPanelWidth() int {
	if m.narrow() {
		return m.width
	}
	return m.sidebarWidth()
}

// overList says whether a screen column falls in the list: left of the log when both fit side by
// side, the whole screen when focus put the list there.
func (m Model) overList(x int) bool {
	if m.narrow() {
		return m.focus == onList
	}
	return m.sidebar && x < m.sidebarWidth()
}

// itemAt returns the item a screen line of the list holds, or -1: the borders, a section title, a
// rule and a route hold none.
func (m Model) itemAt(y int) int {
	_, of := m.listRows(m.listWidth(m.listPanelWidth()), false)
	room := m.height - 3 // the frame takes everything but the footer, borders included
	at := listFrom(len(of), room, slices.Index(of, m.cursor)) + y - 1
	if y < 1 || y > room || at >= len(of) {
		return -1
	}
	return of[at]
}

// listWidth is the room a line of the list has: the frame, minus its borders and the blank along
// them.
func (m Model) listWidth(width int) int { return max(1, width-3) }

// listRows returns the list's lines, each with the item it holds — -1 for a section title, a rule,
// a blank or a route, where the cursor does not stop. A section announces itself with a rule and
// its title; a facade is the title of its own.
func (m Model) listRows(width int, active bool) (rows []string, of []int) {
	// The rule runs one cell from the border, on both sides: it gives air without ever touching it.
	rule := dim.Render(strings.Repeat("─", max(1, width-1)))
	rows = make([]string, 0, len(m.items)*2+8)
	add := func(line string, item int) {
		rows = append(rows, line)
		of = append(of, item)
	}
	for i, it := range m.items {
		switch {
		case it.kind == kindParam && it.group != "":
			add(row([]part{{indent(childIndent) + it.group, dim}}, width, plain, false), -1)
		case it.group != "":
			if i > 0 {
				add("", -1)
				add(rule, -1)
			}
			add(m.groupLine(it.group, width), -1)
		case it.kind == kindIngress:
			add("", -1)
			add(rule, -1)
		}
		add(m.listRow(i, width, active), i)
		if it.kind == kindIngress {
			for _, line := range m.routeRows(it.ing, width) {
				add(line, -1)
			}
		}
	}
	return rows, of
}

// routeRows writes a facade's routing table under its name. They are not items: their log would be
// the facade's, and the cursor would have nothing to do there — it is the map, not a list.
// Redirects are not there, they are not driven.
func (m Model) routeRows(in *core.Ingress, width int) []string {
	out := make([]string, 0, len(in.Cfg.Routes)+1)
	for _, route := range in.Cfg.Routes {
		switch route.Kind() {
		case config.ToApp:
			out = append(out, row([]part{
				{indent(childIndent) + pad(route.Path, m.pathWidth()), plain},
				{"  " + m.appLabel(route.App), plain},
			}, width, plain, false))

		case config.ToProxy:
			// The target has its own line: URLs are long, and that is where they should read in
			// full.
			out = append(out,
				row([]part{{indent(childIndent) + route.Path, plain}}, width, plain, false),
				row([]part{{indent(valueIndent) + route.Proxy.Host(), dim}}, width, plain, false))
		}
	}
	return out
}

// groupLine renders a section's title in grey.
func (m Model) groupLine(group string, width int) string {
	return row([]part{{indent(starWidth) + group, dim}}, width, plain, false)
}

// listRow returns an item's line: a facade lines up with section titles, what the session and the
// apps contain is indented.
func (m Model) listRow(index, width int, active bool) string {
	it := m.items[index]
	on := index == m.cursor
	back := cursorOff
	if active {
		back = cursorOn
	}
	if it.kind == kindParam {
		field, _ := m.session.Form().Field(it.name)
		group, label := field.Group()
		left := []part{{indent(childIndent), plain}, {label, plain}}
		if group != "" {
			left[0].text += indent(2)
		}
		if field.Disabled || field.Readonly {
			left[1].style = dim
		}
		value := []part{{valueOf(field) + " ", dim}}
		if field.Yielded != nil {
			value = append([]part{{"↓ ", colour(yellow)}}, value...)
		}
		if m.input == paramInput && m.draft.name == it.name {
			value = m.editor()
		}
		return row(aligned(width, left, value), width, back, on)
	}

	star := part{indent(starWidth), plain}
	name := plain
	if index == m.selected {
		star, name = part{" ✱ ", accent}, strong
	}
	glyph, state := m.statusOf(it)
	if on {
		state = onCursor(state)
	}

	left := []part{star}
	// The session and the facades are top-level: they line up with section titles. What a section
	// contains is indented.
	if it.kind == kindGroup || it.kind == kindApp {
		left = append(left, part{indent(childIndent - starWidth), plain})
	}
	left = append(left, part{glyph + " ", colour(state)}, part{it.name, name})
	return row(aligned(width, left, m.rightSlot(it)), width, back, on)
}

// editor writes the current input in place of the value, the block cursor where typing inserts.
func (m Model) editor() []part {
	text, at := m.draft.text, m.draft.at
	under := " "
	if at < len(text) {
		under = string(text[at])
	}
	return []part{{"[" + string(text[:at]), plain}, {under, ptyCursor}, {string(text[min(at+1, len(text)):]) + "] ", plain}}
}

// valueOf writes a parameter's value: a flag reads by its dot, the rest by its text.
func valueOf(field config.Field) string {
	switch {
	case field.Type != "flag":
		return field.Value
	case field.Value == "true":
		return "●"
	}
	return "○"
}

// rightSlot says what an app has worth noting right of its name: the time it spends in transition,
// the missing answer at startup. Nothing the rest of the time.
func (m Model) rightSlot(it item) []part {
	switch it.kind {
	case kindSession:
		// The port is the session's first-class information: it reads on its line, and a cross
		// before it says the session does not apply a configuration; the card says why.
		port := part{"port " + strconv.Itoa(m.session.Config().Port) + " ", dim}
		if m.session.NotApplied() != "" {
			return []part{{"✗ ", colour(red)}, port}
		}
		return []part{port}
	case kindApp:
		if glyph, ansi := driftMark(it.app.Drift()); glyph != "" {
			return []part{{glyph + " ", colour(ansi)}}
		}
		if snap := it.app.Snapshot(); snap.Status == core.Starting {
			return []part{{uptime(snap.Since) + " ", dim}}
		}
	}
	return nil
}

// pathWidth aligns the paths of all facades on the same column.
func (m Model) pathWidth() int {
	width := 0
	for _, in := range m.session.Facades() {
		for _, route := range in.Cfg.Routes {
			if route.Kind() == config.ToApp {
				width = max(width, ansi.StringWidth(route.Path))
			}
		}
	}
	return width
}

// sidebarWidth gives the list the room its content asks for — a truncated proxy target would be
// exactly the one you wanted to read — without letting it take more than two fifths of the screen.
// It is measured on the lines rendered with no imposed width: a single layout, so nothing to keep
// in agreement.
func (m Model) sidebarWidth() int {
	natural := 0
	rows, _ := m.listRows(0, false)
	for _, line := range rows {
		natural = max(natural, ansi.StringWidth(line))
	}
	return clamp(natural+4, 26, max(26, m.width*2/5))
}

func indent(width int) string { return strings.Repeat(" ", width) }

// logPanel frames the log. Its name and state live inside, on their own line: no border carries
// text.
func (m Model) logPanel(active bool) []string {
	lines := m.lines()
	screen, at := m.screen(lines)
	from, to, blank := m.logWindow(len(screen))
	rows := make([]string, 0, m.logHeight())
	for range blank {
		rows = append(rows, "")
	}
	for i := from; i < to; i++ {
		rows = append(rows, m.highlighted(screen[i], at[i]))
	}
	// What the parameter under the cursor says covers the oldest lines of the log, rather than
	// trimming its height: that height is what the apps receive as their terminal's size.
	if said := m.paramSays(); len(said) > 0 {
		said = append(said, dim.Render(strings.Repeat("─", max(1, m.logWidth()-1))))
		rows = append(said, rows[min(len(said), len(rows)):]...)
	}
	return panel(m.logHead(len(lines), active), rows, m.logPanelWidth(), m.height-1, active)
}

// paramSays returns what the parameter under the cursor has to say, wrapped to the log's width: its
// reasons if it is disabled, that it is imposed, its disabled choices, the kept value it no longer
// takes, its errors and the form's, and the refusal of the last gesture made on it. Nothing when
// the cursor is not on a parameter, or the list does not have the keyboard.
func (m Model) paramSays() []string {
	if m.focus != onList || m.items[m.cursor].kind != kindParam {
		return nil
	}
	form := m.session.Form()
	field, _ := form.Field(m.items[m.cursor].name)
	var said []string
	say := func(style lipgloss.Style, text string) {
		for _, line := range strings.Split(ansi.Wrap(text, m.logWidth(), ""), "\n") {
			said = append(said, style.Render(line))
		}
	}
	switch {
	case field.Disabled && len(field.Reasons) > 0:
		say(dim, "disabled: "+strings.Join(field.Reasons, "; "))
	case field.Disabled:
		say(dim, "disabled here")
	case field.Readonly:
		say(dim, "read-only here")
	}
	for _, off := range field.Unavailables() {
		say(dim, off)
	}
	if said := field.Yields(); said != "" {
		say(colour(yellow), "↓ "+said)
	}
	for _, notice := range form.Notices {
		say(colour(yellow), notice)
	}
	for _, err := range append(slices.Clone(field.Errors), form.Errors...) {
		say(colour(red), "✗ "+err)
	}
	if m.refusal.name == field.Name && m.refusal.why != "" {
		say(colour(red), "✗ "+m.refusal.why)
	}
	return said
}

// logWindow is the slice of screen lines the panel shows, and the number of empty lines set above
// when the log does not fill it. Rendering and clicking both ask for it.
func (m Model) logWindow(length int) (from, to, blank int) {
	visible := m.logHeight()
	from = max(0, length-visible-m.offset)
	to = max(from, length-m.offset)
	return from, to, visible - (to - from)
}

// spotAt returns the log point a screen cell holds, and says whether there is one: the borders, the
// name line and the emptiness above a short log hold none.
func (m Model) spotAt(x, y int) (spot, bool) {
	_, at := m.screen(m.lines())
	from, to, blank := m.logWindow(len(at))
	i := from + (y - logTop) - blank
	if y < logTop || i < from || i >= to {
		return spot{}, false
	}
	// The first text column comes after the panel's border and the blank along it.
	return spot{line: at[i].line, col: max(0, at[i].col+x-(m.width-m.logPanelWidth())-2)}, true
}

// highlighted lays the selection on a screen line, known to come from a given log line at a given
// column: the selection lives in the log, and a line wrapped in three is only one.
func (m Model) highlighted(line string, at spot) string {
	if m.sel.empty() {
		return line
	}
	first, last := m.sel.span()
	if at.line < first.line || at.line > last.line {
		return line
	}
	from, to := 0, m.logWidth()
	if at.line == first.line {
		from = max(from, first.col-at.col)
	}
	if at.line == last.line {
		to = min(to, last.col+1-at.col)
	}
	return reversed(line, from, to)
}

// logHead names what is being read, with its state at the end of the line.
func (m Model) logHead(total int, active bool) string {
	shown := m.items[m.selected]
	title := strong
	if active {
		title = strong.Foreground(accent.GetForeground())
	}
	glyph, state := m.statusOf(shown)
	return row(aligned(m.logWidth(),
		[]part{{glyph + " ", colour(state)}, {shown.name, title}},
		[]part{{m.logStatus(total), dim}}), m.logWidth(), plain, false)
}

// logStatus returns what is added to the name: the target's state, what the filter and scrolling
// are doing, and the number of lines.
func (m Model) logStatus(total int) string {
	shown := m.items[m.selected]
	parts := []string{}
	switch shown.kind {
	case kindSession:
		parts = append(parts, fmt.Sprintf("port %d", m.session.Config().Port), m.session.Mode().Detail)
	case kindGroup:
		parts = append(parts, plural(len(m.appsOf(shown.name)), "app"))
	case kindIngress:
		snap := shown.ing.Snapshot()
		parts = append(parts, fmt.Sprintf("%d req · %d err", snap.Requests, snap.Failures))
	default:
		if m.input == consoleInput {
			parts = append(parts, "console")
		} else {
			parts = append(parts, status(shown.app))
		}
	}
	if m.filter != "" {
		parts = append(parts, "/"+m.filter)
	}
	if m.offset > 0 {
		parts = append(parts, fmt.Sprintf("−%d", m.offset))
	}
	return strings.Join(append(parts, plural(total, "line")), " · ")
}

// screen returns the log's lines as they are displayed: a line too long takes as many screen lines
// as needed to read it in full, and that is the unit scrolling counts. The console's cursor falls
// on the one where the app actually writes.
func (m Model) screen(lines []core.Line) (rows []string, at []spot) {
	width := m.logWidth()
	cursor, col := -1, 0
	if m.input == consoleInput {
		cursor, col = m.console()
	}
	rows, at = make([]string, 0, len(lines)), make([]spot, 0, len(lines))
	for i, line := range lines {
		folded := fold(m.shown(line), width)
		if i == cursor {
			for len(folded) <= col/width {
				folded = append(folded, "")
			}
			folded[col/width] = withCursor(folded[col/width], col%width)
		}
		for k, row := range folded {
			rows = append(rows, row)
			at = append(at, spot{line: i, col: k * width})
		}
	}
	return rows, at
}

// shown returns a log line as it is displayed before wrapping: its markers painted, and its
// source's name in front when the log merges several. Copying reads that, like the screen.
func (m Model) shown(line core.Line) string {
	text := mark(line.Text)
	if m.items[m.selected].kind != kindApp {
		text = m.sourceLabel(line.Src) + " " + text
	}
	return text
}

// copied returns the selection's text: what is displayed, without its styles or the blank trailing
// at the end of a line, the log's lines joined by a newline — never at the fold of a wrapped line.
func (m Model) copied() string {
	if m.sel.empty() {
		return ""
	}
	first, last := m.sel.span()
	lines := m.lines()
	if last.line >= len(lines) {
		return ""
	}
	out := make([]string, 0, last.line-first.line+1)
	for i := first.line; i <= last.line; i++ {
		text := m.shown(lines[i])
		from, to := 0, ansi.StringWidth(text)
		if i == first.line {
			from = min(first.col, to)
		}
		if i == last.line {
			to = min(last.col+1, to)
		}
		out = append(out, strings.TrimRight(ansi.Strip(ansi.Cut(text, from, max(from, to))), " "))
	}
	return strings.Join(out, "\n")
}

// wordAt returns the extent of the word touching this point — a run of non-blank characters — in
// the text as displayed.
func (m Model) wordAt(at spot) (from, to spot) {
	lines := m.lines()
	if at.line >= len(lines) {
		return at, at
	}
	left, right := word(ansi.Strip(m.shown(lines[at.line])), at.col)
	return spot{at.line, left}, spot{at.line, right}
}

// console says where the displayed app writes in its log: the line, among those lines returns, and
// the column.
func (m Model) console() (row, col int) {
	return m.items[m.selected].app.Log.Cursor()
}

// reversed lays reverse video on a range of columns of a screen line, over the styles it already
// carries — like the console's cursor, under which the text loses its colour.
func reversed(line string, from, to int) string {
	width := ansi.StringWidth(line)
	from, to = clamp(from, 0, width), clamp(to, 0, width)
	if from >= to {
		return line
	}
	return ansi.Truncate(line, from, "") + picked.Render(ansi.Strip(ansi.Cut(line, from, to))) +
		ansi.TruncateLeft(line, to, "")
}

// word returns the columns of the word touching this one, a word being a run of non-blank
// characters. A blank has no word: the extent is then empty.
func word(text string, col int) (from, to int) {
	runes := []rune(text)
	if col < 0 || col >= len(runes) || unicode.IsSpace(runes[col]) {
		return col, col
	}
	from, to = col, col
	for from > 0 && !unicode.IsSpace(runes[from-1]) {
		from--
	}
	for to+1 < len(runes) && !unicode.IsSpace(runes[to+1]) {
		to++
	}
	return from, to
}

// withCursor lays a block cursor at the column where the app writes.
func withCursor(line string, col int) string {
	head := pad(ansi.Truncate(line, col, ""), col)
	rest := ansi.TruncateLeft(line, col, "")
	under, tail := " ", ""
	if runes := []rune(ansi.Strip(rest)); len(runes) > 0 {
		under = string(runes[0])
		tail = ansi.TruncateLeft(rest, 1, "")
	}
	return head + ptyCursor.Render(under) + tail
}

func (m Model) footer() string {
	switch m.input {
	case filterInput:
		return pad(" "+accent.Render("/")+m.filter+cursorOn.Render(" "), m.width)
	case paramInput:
		field, _ := m.session.Form().Field(m.draft.name)
		return pad(" "+cursorOn.Render(" "+m.draft.name+" ")+" "+dim.Render(strings.Join(append(field.Bounds(), ""), " · "))+
			accent.Render("⏎")+dim.Render(" applies · ")+accent.Render("esc")+dim.Render(" cancels"), m.width)
	case consoleInput:
		name := m.items[m.selected].name
		return pad(" "+cursorOn.Render(" console ")+" "+
			dim.Render("what you type goes to ")+colour(m.items[m.selected].app.Cfg().Ansi).Render(name)+
			dim.Render(", arrows included · ")+accent.Render("pgup/pgdn")+dim.Render(" scrolls · ")+
			accent.Render("esc")+dim.Render(" to leave"), m.width)
	}
	keys := [][2]string{{"↑↓/jk", "cursor"}, {"→/space", "show"}}
	if opens := m.opens(); opens != "" {
		keys = append(keys, [2]string{"⏎", opens})
	}
	if m.focus == onLog || m.narrow() {
		keys = [][2]string{{"↑↓/jk", "scroll"}, {"←/esc", "list"}, {"drag", "copy"}, {"/", "filter"}}
		if opens := m.opens(); opens != "" {
			keys = slices.Insert(keys, 2, [2]string{"⏎", opens})
		}
	}
	keys = append(keys, [2]string{"i", "info"}, [2]string{"s/x/r", "start/stop/restart"})
	if !m.narrow() {
		keys = append(keys, [2]string{"b", "list"})
	}
	keys = append(keys, [2]string{"!", "shell"}, [2]string{"?", "help"}, [2]string{"q", "quit"})

	full := make([]string, 0, len(keys))
	short := make([]string, 0, len(keys))
	for _, key := range keys {
		full = append(full, hint(key[0], key[1]))
		short = append(short, accent.Render(key[0]))
	}
	badge := m.badge()
	room := m.width - ansi.StringWidth(badge)
	line := " " + strings.Join(full, dim.Render(" · "))
	if ansi.StringWidth(line) > room {
		line = " " + strings.Join(short, dim.Render(" · "))
	}
	return pad(line, room) + badge
}

// opens says what ⏎ opens on the displayed item. A console only exists for the duration of a run:
// on a stopped app, ⏎ has nothing to open and the footer does not promise it.
func (m Model) opens() string {
	switch shown := m.items[m.selected]; shown.kind {
	case kindApp:
		if shown.app.Snapshot().Status == core.Ready {
			return "console"
		}
	}
	return ""
}

// badge says in a word that the apps are not isolated — the only thing worth seeing without having
// to navigate. The why is in the session's card. On a private network, there is nothing to report.
func (m Model) badge() string {
	out := ""
	if mode := m.session.Mode(); !mode.Isolated {
		out += warn.Render("⚠ "+mode.Name) + " "
	}
	return out
}

// statusOf returns the state glyph and its ANSI colour: the caller decides the shade and the
// background.
func (m Model) statusOf(it item) (glyph string, ansi int) {
	switch it.kind {
	case kindSession:
		// The port is taken for the whole session: it is running by definition.
		return "●", green
	case kindGroup, kindIngress:
		return aggregate(m.appsOf(it.name))
	}
	return statusOfApp(it.app.Snapshot())
}

func statusOfApp(snap core.Snapshot) (glyph string, ansi int) {
	switch snap.Status {
	case core.Ready:
		return "●", green
	case core.Starting, core.Stopping, core.Restarting:
		return "◐", yellow
	}
	if crashed(snap) {
		return "✗", red
	}
	return "○", grey
}

func crashed(snap core.Snapshot) bool {
	last := snap.Last
	return last != nil && !last.ByUser && (last.Code != 0 || last.Signal != "")
}

// aggregate returns the state of a set of apps: the one they share, the transition as soon as one
// moves, and the failure if one remains. An empty set is running — a facade that only proxies waits
// for nobody, and the port is already taken.
func aggregate(apps []*core.App) (glyph string, ansi int) {
	ready, broken := 0, false
	for _, app := range apps {
		snap := app.Snapshot()
		switch snap.Status {
		case core.Ready:
			ready++
		case core.Idle:
			broken = broken || crashed(snap)
		default:
			return "◐", yellow
		}
	}
	switch {
	case broken:
		return "✗", red
	case ready == len(apps):
		return "●", green
	case ready > 0:
		return "◐", yellow
	}
	return "○", grey
}

func (m Model) sourceLabel(src string) string {
	for _, app := range m.session.Apps() {
		if app.Name == src {
			return colour(app.Cfg().Ansi).Render(pad(src, m.sourceWidth()))
		}
	}
	return dim.Render(pad(src, m.sourceWidth()))
}

func (m Model) sourceWidth() int {
	width := 0
	for _, app := range m.session.Apps() {
		width = max(width, ansi.StringWidth(app.Name))
	}
	for _, in := range m.session.Facades() {
		width = max(width, ansi.StringWidth(in.Cfg.Name))
	}
	return width
}

// logPanelWidth is the width of the log's panel: the whole screen, or what the list leaves it.
func (m Model) logPanelWidth() int {
	if m.sidebar && !m.narrow() {
		return m.width - m.sidebarWidth()
	}
	return m.width
}

// logWidth is the room a log line has: the panel, minus its borders and the blank along them. That
// is where lines wrap, and it is the width of the terminal lent to the apps — the two must be the
// same, otherwise an app formats to a width it does not have.
func (m Model) logWidth() int { return max(1, m.logPanelWidth()-3) }

// logHeight is the number of log lines the panel shows: its height, minus its borders, the name
// line and the rule after it.
func (m Model) logHeight() int { return max(1, m.height-1-2-2) }

// modeLines states the network obtained, and what had to be accepted to get it.
func (m Model) modeLines() []string {
	mode := m.session.Mode()
	out := []string{mode.Name + " · " + mode.Detail}
	if mode.Isolated {
		out = append(out, "the apps open no port on the machine")
	} else {
		out = append(out, "no isolation: this is the host network")
	}
	if mode.Reason != "" {
		out = append(out, "fallback: "+mode.Reason)
	}
	return out
}

// helpLines writes the help. leaving is the button that has focus when asking to leave: on a lent
// terminal it is not the same as on one's own, and a help that ignored it would describe the path
// not taken.
func helpLines(leaving string) []string {
	rows := [][2]string{
		{"↑↓ jk", "cursor in the list, scroll in the log"},
		{"→ l ⏎", "shows the item under the cursor, then enters it"},
		{"⏎", "the console of the shown app"},
		{"⏎ space", "on a parameter: its next value"},
		{"⏎", "on a text or a number: edits it, ⏎ applies, esc cancels"},
		{"i", "card of the item; ↑↓ moves through the cards"},
		{"space", "shows without leaving the list"},
		{"← h esc", "back to the list; esc closes the card first"},
		{"click drag", "click shows an item or enters the log; drag in the log copies it"},
		{"s x r", "start · stop · restart of what the focus names"},
		{"b", "opens or closes the list (wide screen)"},
		{"g G", "top · bottom of the log, pgup/pgdown by page"},
		{"/ c", "filters the log · clears it"},
		{"o", "opens the url in the browser"},
		{"R", "rereads the configuration, which saving already rereads"},
		{"u", "takes the binary on disk, keeping the apps"},
		{"!", "a shell in the network of the apps"},
		{"q", "asks how to leave, on “" + leaving + "” — except in the console"},
	}
	width := 0
	for _, row := range rows {
		width = max(width, ansi.StringWidth(row[0]))
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, dim.Render(pad(row[0], width+2))+row[1])
	}
	return out
}

// panel frames a content: always a thin line, and the colour says who has the keyboard. Nothing is
// written on a border — a title lives inside, on its own line, and a border separates it from the
// rest.
func panel(head string, body []string, width, height int, active bool) []string {
	edge := dim
	if active {
		edge = accent
	}
	inner := width - 2
	side := edge.Render("│")
	out := make([]string, 0, height)
	out = append(out, edge.Render("╭"+strings.Repeat("─", inner)+"╮"))
	if head != "" {
		out = append(out, side+" "+pad(head, inner-1)+side,
			edge.Render("├"+strings.Repeat("─", inner)+"┤"))
	}
	room := height - len(out) - 1 // what remains under the title, the bottom border aside
	for _, line := range body[:min(len(body), room)] {
		out = append(out, side+" "+pad(line, inner-1)+side)
	}
	for len(out) < height-1 {
		out = append(out, side+strings.Repeat(" ", inner)+side)
	}
	return append(out, edge.Render("╰"+strings.Repeat("─", inner)+"╯"))
}

// box sizes a floating panel on its content, surrounds it with blank space and centres it. Its
// title and keys live inside, on their own line.
func (m Model) box(title, keys string, body []string) float {
	content := ansi.StringWidth(title) + ansi.StringWidth(keys) + 4
	for _, line := range body {
		content = max(content, ansi.StringWidth(line))
	}
	width := min(content+boxMargin, m.width-2)

	head := strong.Render(title)
	if keys != "" {
		head = pad(head, width-boxMargin-ansi.StringWidth(keys)) + dim.Render(keys)
	}
	// The frame costs two borders, the title line surrounded by blank space, and the bottom margin.
	room := m.height - 2 - 6
	if len(body) > room {
		body = append(body[:max(0, room-1):max(0, room-1)], dim.Render("…"))
	}
	lines := []string{"", head, ""}
	lines = append(lines, body...)
	lines = append(lines, "")

	padded := make([]string, 0, len(lines))
	for _, line := range lines {
		padded = append(padded, "  "+line)
	}
	height := min(len(padded)+2, m.height-2)
	return float{
		lines: panel("", padded, width, height, true),
		x:     max(0, (m.width-width)/2),
		y:     max(0, (m.height-height)/2),
	}
}

// buttons returns the confirmation's buttons, the one with focus filled with the accent.
func (m Model) buttons() string {
	labels := []string{cancelButton: "Cancel"}
	for _, choice := range m.quits {
		labels = append(labels, choice.label)
	}
	rendered := make([]string, 0, len(labels))
	for i, label := range labels {
		style := secondary
		if i == m.button {
			style = primary
		}
		rendered = append(rendered, style.Render("  "+label+"  "))
	}
	return strings.Join(rendered, "  ")
}

// mark colours the lines the core prefixed: the launched command, an incident, the end of a run.
func mark(text string) string {
	switch {
	case strings.HasPrefix(text, "$ "):
		return accent.Render(text)
	case strings.HasPrefix(text, "!! "):
		return bad.Render(text)
	case strings.HasPrefix(text, "-- "):
		return dim.Render(text)
	}
	return text
}

// status says where an app stands: its process, and what awaits it — a declaration it has not taken
// yet, or its removal at the next stop.
func status(app *core.App) string {
	out := describe(app.Snapshot())
	if mark := drift(app); mark != "" {
		out += " · " + mark
	}
	return out
}

// drift paints the gap between the process and the current declaration, glyph included: it is the
// same glyph as in the list, and it never says the process's state, which has its own.
func drift(app *core.App) string {
	glyph, ansi := driftMark(app.Drift())
	if glyph == "" {
		return ""
	}
	return colour(ansi).Render(glyph + " " + app.Drift().String())
}

func driftMark(d core.Drift) (glyph string, ansi int) {
	switch d {
	case core.Stale:
		return "↻", yellow
	case core.Dropped:
		return "⊘", magenta
	}
	return "", 0
}

func describe(snap core.Snapshot) string {
	switch snap.Status {
	case core.Ready:
		return "ready · " + uptime(snap.ReadyAt)
	case core.Starting, core.Stopping, core.Restarting:
		return snap.Status.String()
	}
	if snap.Last != nil {
		return snap.Last.String()
	}
	return "stopped"
}

// section returns a group of lines under its title; an empty group has no title.
func section(title string, values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string{dim.Render(title)}, values...)
}

// sections separates sections with a blank: in a panel already framed, one more line only adds a
// line to read.
func sections(sets ...[]string) []string {
	var out []string
	for _, set := range sets {
		if len(set) == 0 {
			continue
		}
		if out != nil {
			out = append(out, "")
		}
		out = append(out, set...)
	}
	return out
}

func env(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for k, v := range values {
		parts = append(parts, k+"="+v)
	}
	slices.Sort(parts)
	return strings.Join(parts, "  ")
}

func hint(keys, label string) string {
	return accent.Render(keys) + " " + dim.Render(label)
}

func plural(n int, word string) string {
	if n != 1 {
		return strconv.Itoa(n) + " " + word + "s"
	}
	return strconv.Itoa(n) + " " + word
}

func uptime(since time.Time) string {
	if since.IsZero() {
		return "—"
	}
	d := time.Since(since).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func pad(s string, width int) string {
	s = truncate(s, width)
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

func truncate(s string, width int) string {
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// fold wraps a line to the given width, as a terminal would: at the column, without looking for
// words — that is what keeps a character's column computable, and so the console's cursor in its
// place. Each chunk closes the styles it opened: a screen line must leak nothing onto its
// neighbours.
func fold(line string, width int) []string {
	total := ansi.StringWidth(line)
	if total <= width {
		return []string{line}
	}
	rows := make([]string, 0, (total+width-1)/width)
	for at := 0; at < total; at += width {
		row := ansi.Cut(line, at, at+width)
		if strings.ContainsRune(row, ansi.ESC) {
			row += "\x1b[0m"
		}
		rows = append(rows, row)
	}
	return rows
}
