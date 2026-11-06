package panel

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"

	"dsvs/internal/attach"
	"dsvs/internal/build"
	"dsvs/internal/config"
)

// The panel's measures, in pixels. A single column, fixed-height rows: what decides the size is the
// number of apps.
const (
	width        = 420
	pad          = 16
	bleed        = 8
	headerRow    = 36
	titleRow     = 34
	rowHeight    = 28
	errorRow     = 18
	facadeRow    = 24
	actionRow    = 40
	ruleRow      = 17
	newsRow      = 22
	buttonHeight = 24
	buttonPadX   = 10
	buttonGap    = 6
	glyphX       = 24
	nameX        = 38
	statusX      = 190
)

// The palette. Every text/background or glyph/background pair it allows has its contrast measured by
// a test.
var (
	ground     = rgb(0x1c1d22)
	rule       = rgb(0x2e3038)
	rowLit     = rgb(0x2a2c35)
	key        = rgb(0x3b3e49)
	keyLit     = rgb(0x4a4e5e)
	keyDown    = rgb(0x2a2c34)
	ink        = rgb(0xe8e8ec)
	faint      = rgb(0x9a9dab)
	subdued    = rgb(0xaeb1bd)
	brokenText = rgb(0xef8a8a)
	accent     = rgb(0x3566c8)
	accentLit  = rgb(0x3d6fd6)
	accentDown = rgb(0x2d58ae)
	white      = rgb(0xffffff)
	ready      = rgb(0x6fc276)
	waking     = rgb(0xe0b34a)
	broken     = rgb(0xe06c6c)
	asleep     = rgb(0x7c7f8c)
	branch     = rgb(0xb9a3ff)
)

func rgb(hex uint32) color.RGBA {
	return color.RGBA{uint8(hex >> 16), uint8(hex >> 8), uint8(hex), 0xff}
}

// deed is what a click triggers. Zones carry them; the window executes them. Those that are session
// orders carry the verb the session understands: there is nothing to translate between the click and
// the order.
type deed string

const (
	nothing deed = ""
	dismiss deed = "dismiss"

	// A session's orders, spelled as it understands them.
	start    deed = "start"
	stop     deed = "stop"
	restart  deed = "restart"
	closeAll deed = "quit"

	// openTUI opens the session's interface or brings it back on screen, open starts the session of a
	// project already seen, forget forgets it once its apps are off, stopEverything stops the apps of
	// every project, and quitDsvs goes away with the icon.
	openTUI        deed = "open the tui"
	open           deed = "wake"
	forget         deed = "forget"
	stopEverything deed = "stop everything"
	quitDsvs       deed = "quit dsvs"

	// browse opens a facade in the default browser, unfold unfolds the project whose summary alone is
	// shown, drop unfolds a parameter's list of values, or folds it.
	browse deed = "browse"
	unfold deed = "unfold"
	drop   deed = "drop"

	// choose gives a parameter the value the zone carries, and sets a parameter of the session it
	// names; edit opens a text or a number for entry, the zone carrying its value.
	choose deed = "choose"
	edit   deed = "edit"

	// look searches for a new version of the binary, renew takes it.
	look  deed = "look"
	renew deed = "renew"

	// installBeta puts dsvs-beta beside dsvs, removeBeta takes it away from its own panel. Each is its
	// button's label.
	installBeta deed = "install the beta"
	removeBeta  deed = "uninstall the beta"
)

// project is a panel row: a project already seen, what of it runs when it runs, and what a gesture
// gave when it failed — the reason reads where the gesture took place.
type project struct {
	config string
	live   *attach.State
	failed string
	// waking says its session is coming up: there is then nothing to click on its row, and that is
	// what the row says. It only goes with a cold project: the session coming up has no state yet.
	waking bool
	// pending says, per app with an order on its way, until when it waits: it offers nothing to click
	// before its session's next state, otherwise a second click, which would change meaning with the
	// state, would do the opposite of what one expects. The wait ends even if the order changes
	// nothing.
	pending map[string]time.Time
	// replaying is the parameter change the session is replaying, nil when it replays none: a value
	// is changed one at a time, and the finished replay — successful or refused — hands control back.
	replaying *change
	// refused is the reason for the last change the session refused.
	refused string
}

// change is a value given to a parameter, while the session replays its configuration.
type change struct{ name, value string }

// renewal is what the panel knows of the update. Three states and not one more: it is working and
// shows its step, it found a version and offers to take it, or it has something to say and offers to
// search again. The working state is built by doing, which requires its step: that is what rules out
// a panel with nothing to show or to click.
type renewal struct {
	stage stage
	said  string
	// to is the version found, and only means something under found.
	to string
	// failed says said is the reason for a failure, and only means something under idle: failure sets
	// it.
	failed bool
	// beta is the gesture offered on the beta outside of work, and betaOut the beta installBeta takes.
	beta    deed
	betaOut string
}

// stage is where the update stands. Zero is the working state: nothing to click, and said tells the
// step.
type stage int

const (
	working stage = iota
	found
	idle
)

// failure is the update that did not succeed: its reason reads as an error, and one can search again.
func failure(err error) renewal { return renewal{stage: idle, said: err.Error(), failed: true} }

// doing is the update at work: it says where it stands, and there is nothing to click while it is.
func doing(said string) renewal { return renewal{stage: working, said: said} }

// zone is a region of the panel and what a click does there. config names the session concerned, name
// what the order designates in it: the app for an app order, the URL for browse.
type zone struct {
	at     image.Rectangle
	do     deed
	config string
	name   string
	// value is the value choose gives to the parameter name.
	value string
}

// faces are the panel's fonts: the text, the titles, the discreet labels, and the fixed pitch of the
// ports.
type faces struct {
	text  font.Face
	bold  font.Face
	small font.Face
	mono  font.Face
}

// view is how the panel is being looked at: where the pointer is, outside the image when it is
// elsewhere, which zone it started pressing, the empty zone when none is, what height the screen
// leaves, the project chosen to unfold when not everything fits, the parameter whose list of values
// is unfolded, the one being typed into, and the one whose entry did not get the keyboard — only one
// of each at a time.
type view struct {
	at       image.Point
	pressed  zone
	room     int
	unfolded string
	dropped  slot
	typing   draft
	keyless  slot
}

// slot designates a session's parameter; the zero value designates none.
type slot struct{ config, name string }

// draft is the entry of a text or a number: the parameter it targets, the zero value targeting none,
// what was typed, and the cursor's place in that text. Nothing leaves before Enter.
type draft struct {
	slot
	text []rune
	at   int
}

// paint draws the sessions and returns the image to push, the zones to click, and the app rows as it
// laid them out. What is under the pointer lights up, so that one sees what one is about to click —
// and the drawing says where its rows are, so the window does not have to redo its geometry to know
// when the hover changed. Nothing X11 here.
func paint(projects []project, news renewal, with faces, now time.Time, seen view) (*image.RGBA, []zone, []image.Rectangle) {
	p := compose(projects, news, with, now, seen, "")
	if p.y > seen.room {
		p = compose(projects, news, with, now, seen, kept(projects, seen.unfolded))
	}
	return p.cropped(), p.zones, p.rows
}

// compose lays out the whole panel, except the running projects that kept does not name: folded into
// a one-line summary. Without kept, nothing is folded.
func compose(projects []project, news renewal, with faces, now time.Time, seen view, kept string) *painter {
	p := &painter{view: seen, with: with, now: now}
	p.stretch(headerRow + 2*ruleRow + actionRow)

	p.header()
	p.line()
	if len(projects) == 0 {
		top := p.reserve(56)
		p.write(pad, top+22, "No project yet", with.bold, faint)
		p.write(pad, top+42, "Run "+build.Name()+" in a folder with a "+config.Names[0]+".", with.text, faint)
	}
	named := labels(projects)
	for i, one := range projects {
		if i > 0 {
			p.line()
		}
		if one.live != nil && kept != "" && one.config != kept {
			p.summary(one, named[one.config])
			continue
		}
		p.project(one, named[one.config])
	}
	p.line()
	p.global(projects, news)
	p.reserve(4)
	return p
}

// kept is the project that stays unfolded when not everything fits: the chosen one if it is still
// running, otherwise the first one with a fallen app, failing that the first one starting, failing
// that the first one running. A cold project already fits on one line, it does not enter the choice.
func kept(projects []project, chosen string) string {
	living := slices.DeleteFunc(slices.Clone(projects), func(one project) bool { return one.live == nil })
	if slices.ContainsFunc(living, func(one project) bool { return one.config == chosen }) {
		return chosen
	}
	for _, wanted := range []glyph{cross, half} {
		for _, one := range living {
			if slices.ContainsFunc(one.live.Apps, func(app attach.App) bool { shape, _ := form(app); return shape == wanted }) {
				return one.config
			}
		}
	}
	if len(living) > 0 {
		return living[0].config
	}
	return ""
}

// painter carries the drawing in progress: the canvas, the line being written, and the zones already
// laid out.
type painter struct {
	view
	canvas *image.RGBA
	with   faces
	now    time.Time
	y      int
	zones  []zone
	// rows are the app rows, in the order they are painted: they are what hover lights up, and the
	// window uses them to know when it changed row.
	rows []image.Rectangle
}

// reserve takes the next strip and returns its top edge. It is the only place that moves the drawing
// forward, hence the only one that decides the panel's height: nobody has to know it in advance, and
// a geometry written twice has nowhere left to contradict itself.
func (p *painter) reserve(height int) int {
	top := p.y
	p.y += height
	if p.y > p.canvas.Rect.Dy() {
		p.stretch(max(2*p.canvas.Rect.Dy(), p.y))
	}
	return top
}

// stretch remakes the canvas taller, background laid, and copies back what is already drawn.
func (p *painter) stretch(height int) {
	taller := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(taller, taller.Bounds(), image.NewUniform(ground), image.Point{}, draw.Src)
	if p.canvas != nil {
		copy(taller.Pix, p.canvas.Pix)
	}
	p.canvas = taller
}

// cropped returns the panel at the height reached. It shares the working canvas's pixels: what was
// drawn is exactly what is pushed.
func (p *painter) cropped() *image.RGBA {
	return &image.RGBA{Pix: p.canvas.Pix[:p.y*p.canvas.Stride], Stride: p.canvas.Stride,
		Rect: image.Rect(0, 0, width, p.y)}
}

// header writes the binary this panel comes from — its name and version, which tells two dsvs apart
// at a glance — and its cross. There is always a cross: it is the way out that depends on nothing —
// neither a key, nor the focus, nor a click elsewhere, which this machine's X server would not even
// see.
func (p *painter) header() {
	top := p.reserve(headerRow)
	labelX := pad
	if build.Name() != build.Stable {
		p.round(image.Rect(pad, top+13, pad+10, top+23), 2, branch)
		labelX += 16
	}
	p.write(labelX, top+22, build.Label(), p.with.small, faint)
	close := zone{at: image.Rect(width-pad-buttonHeight, top+6, width-pad, top+6+buttonHeight), do: dismiss}
	p.round(close.at, 4, p.tone(close, ground, keyLit, keyDown))
	p.pictogram(dismiss, close.at.Min.X+buttonHeight/2, close.at.Min.Y+buttonHeight/2, ink)
	p.zones = append(p.zones, close)
}

// project writes a project: its title, what of it runs, and what can be done to it. Cold, there is
// only its path and a way to start it — that is what remains of a project between two sessions.
func (p *painter) project(one project, named string) {
	top := p.reserve(titleRow)
	cold := one.live == nil
	nameTint, right := ink, width-pad
	switch {
	case one.waking:
		nameTint, right = faint, right-p.widthOf(starting, p.with.text)
		p.write(right, top+22, starting, p.with.text, faint)
		right -= 8
	case cold:
		// Cold, both gestures move up to the title line: a sleeping project fits on one line.
		nameTint = faint
		right = p.button(top+6, right, "forget", forget, one.config, "")
		right = p.button(top+6, right, "start", open, one.config, "")
	default:
		port := ":" + strconv.Itoa(one.live.Port)
		right -= p.widthOf(port, p.with.mono)
		p.write(right, top+22, port, p.with.mono, faint)
		right -= 8
	}
	p.write(pad, top+22, p.fitting(named, right-pad-8, p.with.bold), p.with.bold, nameTint)

	p.reason(nameX, one.failed)
	if cold {
		p.reserve(4)
		return
	}
	p.stale(one.live.NotApplied)
	for _, app := range one.live.Apps {
		p.app(one, app)
	}
	p.facades(one.config, one.live.Facades, one.live.Port)
	p.params(one)
	p.actions(one)
}

// reason writes a failure's reason under a state cross, the text at x.
func (p *painter) reason(x int, text string) {
	top := p.y
	if p.note(x, text, brokenText) {
		p.mark(x-nameX+glyphX, top+errorRow/2, cross, broken)
	}
}

// note writes a message under what it concerns, and says whether there was one. It comes from
// elsewhere and has no promised length: it takes the lines it needs rather than ending in an
// ellipsis, the panel having no height set in advance. That is the rule for every message — a
// failure, a configuration the session does not apply, a disabled field's reasons, its errors.
func (p *painter) note(x int, text string, tint color.Color) bool {
	lines := wrapped(text, width-pad-x, func(line string) int { return p.widthOf(line, p.with.text) })
	for _, line := range lines {
		p.write(x, p.reserve(errorRow)+14, line, p.with.text, tint)
	}
	if len(lines) > 0 {
		p.reserve(6)
	}
	return len(lines) > 0
}

// stale writes why the session keeps the previous configuration.
func (p *painter) stale(why string) {
	if why != "" {
		p.reason(nameX, notApplied+": "+why)
	}
}

// summary writes a folded project: its name, how many apps are in each state — each count behind its
// shape — its port, and nothing else. A click unfolds it. A failure's reason reads by unfolding, and
// a lone cross, without a count, says there is one.
func (p *painter) summary(one project, named string) {
	top := p.reserve(titleRow)
	z := zone{at: image.Rect(bleed, top+2, width-bleed, top+titleRow-2), do: unfold, config: one.config}
	p.round(z.at, 4, p.tone(z, ground, rowLit, keyDown))
	p.zones = append(p.zones, z)
	p.pictogram(unfold, pad+4, top+17, faint)

	port := ":" + strconv.Itoa(one.live.Port)
	x := width - pad - p.widthOf(port, p.with.mono)
	p.write(x, top+22, port, p.with.mono, faint)
	x -= 14
	if one.failed != "" || one.live.NotApplied != "" {
		x -= 7
		p.mark(x, top+17, cross, broken)
		x -= 7 + 14
	}

	counts, tints := map[glyph]int{}, map[glyph]color.Color{}
	for _, app := range one.live.Apps {
		shape, tint := form(app)
		counts[shape]++
		tints[shape] = tint
	}
	for _, shape := range []glyph{ring, filled, half, cross} {
		if counts[shape] == 0 {
			continue
		}
		count := strconv.Itoa(counts[shape])
		x -= p.widthOf(count, p.with.mono)
		p.write(x, top+22, count, p.with.mono, faint)
		x -= 10
		p.mark(x, top+17, shape, tints[shape])
		x -= 14
	}
	p.write(pad+16, top+22, p.fitting(named, x-pad-16-8, p.with.bold), p.with.bold, ink)
}

// facadePadX is the breathing room around a host: it is what gives its zone something to be clicked.
const facadePadX = 6

// facades writes the hosts this session serves, one by one and clickable: they are the links one
// opens, and a host cut in the middle can be neither read nor typed. So they wrap to the next line
// rather than being elided, like the failure reason above — the panel has no height set in advance.
// Only a host too long for a whole line ends in an ellipsis.
func (p *painter) facades(config string, facades []attach.Facade, port int) {
	const left = nameX - facadePadX
	right := width - pad
	// x at the right edge: the first host wraps there, and with no facade no line is taken.
	top, x := 0, right
	for _, facade := range facades {
		host := p.fitting(facade.Host, right-left-facadePadX*2, p.with.text)
		wide := p.widthOf(host, p.with.text) + 2*facadePadX
		if x+wide > right {
			top, x = p.reserve(facadeRow), left
		}
		z := zone{at: image.Rect(x, top, x+wide, top+facadeRow), do: browse, config: config, name: url(facade.Host, port)}
		tint := faint
		if p.at.In(z.at) {
			tint = ink
		}
		p.round(z.at.Inset(1), 4, p.tone(z, ground, keyLit, keyDown))
		p.write(x+facadePadX, top+17, host, p.with.text, tint)
		draw.Draw(p.canvas, image.Rect(x+facadePadX, top+19, z.at.Max.X-facadePadX, top+20),
			image.NewUniform(tint), image.Point{}, draw.Src)
		p.zones = append(p.zones, z)
		x = z.at.Max.X + 2
	}
}

// params writes the session's parameters in the form's order, each group under its title, then what
// the machine shows of itself, then the form's errors and the reason for the last refused change.
// What is observed offers nothing to click — the grammar says so on its own, nobody sets the network
// from here — and, while a session replays a change, neither do the parameters: that is what keeps a
// second replay from crossing a first. A hidden field does not exist here, and a group whose fields
// are all hidden therefore has no title.
func (p *painter) params(one project) {
	state := one.live
	if len(state.Params) == 0 && state.Observed == nil {
		return
	}
	p.reserve(6)
	group := ""
	for _, field := range state.Params {
		if field.Hidden {
			continue
		}
		if within, _ := field.Group(); within != group {
			group = within
			if group != "" {
				p.write(nameX, p.reserve(titleHeight)+15, p.fitting(group, width-pad-nameX, p.with.small), p.with.small, faint)
			}
		}
		p.param(one, field)
	}
	p.observed(state.Observed)
	for _, notice := range state.Notices {
		p.note(nameX, notice, waking)
	}
	for _, err := range state.Errors {
		p.reason(nameX, err)
	}
	p.reason(nameX, one.refused)
}

// titleHeight is the strip of a parameter group's title.
const titleHeight = 20

// param writes a parameter: its label on the left, indented under its group's title, its control
// aligned right, then what it has to say. Disabled, its control is inert, as during a replay, and its
// reasons are written below; an imposed value reads like what is observed, without being greyed out.
// A kept value that no longer holds is written in amber under the one replacing it, while it yields.
func (p *painter) param(one project, param config.Field) {
	group, label := param.Group()
	labelX := nameX
	if group != "" {
		labelX += groupIndent
	}
	if param.Readonly {
		p.fact(labelX, label, param.Value, ink, "", faint)
		p.reason(labelX, strings.Join(param.Errors, "; "))
		return
	}
	top := p.reserve(rowHeight)
	row := image.Rect(bleed, top, width-bleed, top+rowHeight)
	shown := param.Value
	if one.replaying != nil && one.replaying.name == param.Name {
		shown = one.replaying.value
	}
	inert := one.replaying != nil || param.Disabled
	typing := !inert && p.typing.slot == slot{one.config, param.Name}
	if p.at.In(row) && !inert && !typing {
		p.round(image.Rect(row.Min.X, row.Min.Y+1, row.Max.X, row.Max.Y-1), 4, rowLit)
	}
	var left int
	widths, segmented := p.segmentWidths(param)
	switch {
	case typing:
		left = p.editor(top)
	case param.Type == "flag":
		left = p.toggle(row, one.config, param, shown, inert)
	case param.Choices == nil:
		left = p.textbox(top, one.config, param, shown, inert)
	case segmented:
		left = p.segments(top, one.config, param, shown, widths, inert)
	default:
		left = p.picker(top, one.config, param, shown, inert)
	}
	if one.replaying != nil {
		left -= 8 + p.widthOf(applying, p.with.text)
		p.write(left, top+19, applying, p.with.text, faint)
	}
	p.write(labelX, top+19, p.fitting(label, left-8-labelX, p.with.text), p.with.text, faint)
	if p.dropped == (slot{one.config, param.Name}) && !(segmented || param.Type == "flag" || param.Choices == nil) && !inert {
		p.choices(one.config, param, shown)
	}
	if p.keyless == (slot{one.config, param.Name}) {
		p.note(labelX, "no keyboard here: "+build.Name()+" set "+param.Name+"=… or the TUI", faint)
	}
	if typing {
		p.note(labelX, strings.Join(append(param.Bounds(), "enter applies, esc cancels"), " · "), faint)
	}
	if param.Disabled {
		p.note(labelX, strings.Join(param.Reasons, "; "), faint)
	}
	p.note(labelX, strings.Join(param.Unavailables(), "; "), faint)
	p.note(labelX, param.Yields(), waking)
	p.reason(labelX, strings.Join(param.Errors, "; "))
}

// groupIndent is a field's indent under its group's title.
const groupIndent = 12

// textbox writes a text or a number: its value in a box, which a click opens for entry on the line
// itself. Returns the box's left edge.
func (p *painter) textbox(top int, session string, param config.Field, shown string, inert bool) int {
	text := p.fitting(shown, 150, p.with.text)
	box := image.Rect(width-pad-max(48, p.widthOf(text, p.with.text)+2*buttonPadX), top+3, width-pad, top+25)
	z := zone{at: image.Rect(box.Min.X, top, box.Max.X, top+rowHeight), do: edit, config: session, name: param.Name, value: shown}
	tint, fill := asleep, key
	if !inert {
		tint, fill = subdued, p.tone(z, key, keyLit, keyDown)
		if p.at.In(z.at) {
			tint = ink
		}
		p.zones = append(p.zones, z)
	}
	p.round(box, 4, fill)
	p.write(box.Max.X-buttonPadX-p.widthOf(text, p.with.text), top+19, text, p.with.text, tint)
	return box.Min.X
}

// editor writes the entry in progress: a wider box, outlined with the accent, where the text follows
// the cursor when it does not fit. It is not a zone: one types in it, one does not click it. Returns
// the box's left edge.
func (p *painter) editor(top int) int {
	box := image.Rect(nameX+labelRoom, top+3, width-pad, top+25)
	p.round(box, 5, accent)
	p.round(box.Inset(1), 4, keyDown)
	room, text := box.Dx()-2*buttonPadX, p.typing.text
	from := 0
	for p.widthOf(string(text[from:p.typing.at]), p.with.text) > room {
		from++
	}
	p.write(box.Min.X+buttonPadX, top+19, p.fitting(string(text[from:]), room, p.with.text), p.with.text, ink)
	caret := box.Min.X + buttonPadX + p.widthOf(string(text[from:p.typing.at]), p.with.text)
	draw.Draw(p.canvas, image.Rect(caret, top+7, caret+1, top+21), image.NewUniform(ink), image.Point{}, draw.Src)
	return box.Min.X
}

// applying is what is written before each control while a session replays.
const applying = "applying…"

// segmentWidths returns the width of each segment of a select, and says whether it fits as segments:
// at most four values, in the room the label leaves.
func (p *painter) segmentWidths(param config.Field) ([]int, bool) {
	widths, total := make([]int, len(param.Choices)), 4
	for i, choice := range param.Choices {
		widths[i] = max(buttonHeight, p.widthOf(choice, p.with.text)+16)
		total += widths[i]
	}
	return widths, len(widths) <= 4 && total <= width-pad-nameX-labelRoom
}

// segments writes a select with short values: all in view within one frame, the chosen one filled.
// Every other segment is a zone over the row's full height; the chosen one is not, there is nothing
// to change there. Returns the frame's left edge.
func (p *painter) segments(top int, session string, param config.Field, shown string, widths []int, inert bool) int {
	total := 4
	for _, w := range widths {
		total += w
	}
	x := width - pad - total
	p.round(image.Rect(x, top+3, width-pad, top+25), 5, key)
	left := x
	x += 2
	for i, choice := range param.Choices {
		box := image.Rect(x, top+5, x+widths[i], top+23)
		x = box.Max.X
		fill, text := key, subdued
		switch {
		case choice == shown && inert:
			fill, text = keyLit, ink
		case choice == shown:
			fill, text = accent, white
		case inert || param.Unavailable(choice) != "":
			text = asleep
		default:
			z := zone{at: image.Rect(box.Min.X, top, box.Max.X, top+rowHeight), do: choose,
				config: session, name: param.Name, value: choice}
			fill = p.tone(z, key, keyLit, keyDown)
			if p.at.In(z.at) {
				text = ink
			}
			p.zones = append(p.zones, z)
		}
		p.round(box, 4, fill)
		p.write(box.Min.X+(widths[i]-p.widthOf(choice, p.with.text))/2, top+19, choice, p.with.text, text)
	}
	return left
}

// picker writes a select with long or many values: a button carrying the chosen value and a chevron;
// a click unfolds the values under the row, another folds them. Returns the button's left edge.
func (p *painter) picker(top int, session string, param config.Field, shown string, inert bool) int {
	text := p.fitting(shown, 150, p.with.text)
	box := image.Rect(width-pad-(p.widthOf(text, p.with.text)+buttonPadX+28), top+3, width-pad, top+25)
	z := zone{at: image.Rect(box.Min.X, top, box.Max.X, top+rowHeight), do: drop, config: session, name: param.Name}
	tint, fill := asleep, key
	if !inert {
		tint, fill = subdued, p.tone(z, key, keyLit, keyDown)
		if p.at.In(z.at) {
			tint = ink
		}
		p.zones = append(p.zones, z)
	}
	p.round(box, 4, fill)
	p.write(box.Min.X+buttonPadX, top+19, text, p.with.text, tint)
	p.pictogram(drop, box.Max.X-14, top+14, tint)
	return box.Min.X
}

// choices writes the values of an unfolded select, one row per value, in the panel itself: it grows,
// which it already knows how to do. Choosing a value gives it to the parameter; the chosen one folds
// the list.
func (p *painter) choices(session string, param config.Field, shown string) {
	for _, choice := range param.Choices {
		top := p.reserve(26)
		z := zone{at: image.Rect(bleed, top, width-bleed, top+26), do: choose, config: session, name: param.Name, value: choice}
		tint := faint
		switch {
		case choice == shown:
			z.do, tint = drop, ink
		case param.Unavailable(choice) != "":
			tint = asleep
		}
		if tint != asleep {
			p.round(z.at, 4, p.tone(z, ground, rowLit, keyDown))
			p.zones = append(p.zones, z)
		}
		if choice == shown {
			p.pictogram(choose, 44, top+13, ink)
		}
		p.write(58, top+18, p.fitting(choice, width-pad-58, p.with.text), p.with.text, tint)
	}
}

// toggle writes a flag: a track whose knob says the state, the color repeating it. A click anywhere
// on the row toggles. Returns the track's left edge.
func (p *painter) toggle(row image.Rectangle, session string, param config.Field, shown string, inert bool) int {
	track := image.Rect(width-pad-32, row.Min.Y+5, width-pad, row.Min.Y+23)
	param.Value = shown
	z := zone{at: row, do: choose, config: session, name: param.Name, value: param.Next()}
	on := shown == "true"
	rest, lit, down, knob, knobX := key, keyLit, keyDown, faint, track.Min.X+9
	if on {
		rest, lit, down, knob, knobX = accent, accentLit, accentDown, white, track.Max.X-9
	}
	fill := key
	if inert {
		knob = asleep
	} else {
		fill = p.tone(z, rest, lit, down)
		p.zones = append(p.zones, z)
	}
	p.round(track, 9, fill)
	p.fill(track, knob, func(n pen) { n.circle(float32(knobX), float32(track.Min.Y+9), 7, solid) })
	return track.Min.X
}

// observed writes what the machine shows: the network in ink, followed by "detected". Nothing
// observed reads faint, without a suffix.
func (p *painter) observed(seen *config.Observed) {
	if seen == nil {
		p.fact(nameX, "network", seen.String(), faint, "", faint)
		return
	}
	p.fact(nameX, "network", seen.String(), ink, detected, faint)
}

// fact writes a read-only row: its label on the left, its value aligned right, and a suffix after it
// when there is one. A value that does not fit beside its label goes below and wraps over the lines
// it needs, like a message: nothing sets it from here, so this is the only place it reads. It offers
// nothing to click.
func (p *painter) fact(labelX int, label, value string, tint color.Color, suffix string, suffixTint color.Color) {
	top := p.reserve(rowHeight)
	right := width - pad
	if suffix != "" {
		right -= p.widthOf(suffix, p.with.small)
		p.write(right, top+19, suffix, p.with.small, suffixTint)
		right -= 8
	}
	beside := labelX+p.widthOf(label, p.with.text)+8+p.widthOf(value, p.with.text) <= right
	if beside {
		right -= p.widthOf(value, p.with.text)
		p.write(right, top+19, value, p.with.text, tint)
		right -= 8
	}
	p.write(labelX, top+19, p.fitting(label, right-labelX, p.with.text), p.with.text, faint)
	if !beside {
		p.note(labelX, value, tint)
	}
}

// detected says a value is not set from here: the machine shows it, and that is all. notApplied opens
// the line of a configuration the session does not apply.
const (
	detected   = "detected"
	notApplied = "not applied"
)

// labelRoom is the room a parameter's label keeps at least, whatever its control asks of it.
const labelRoom = 120

// app writes an app row: its shape, its name, what it is doing, and two fixed slots for the
// pictograms that drive it — the first starts or stops, the second restarts, empty for a stopped app.
// A pictogram therefore never moves under the pointer. The shape says the state without depending on
// a glyph a font might lack.
func (p *painter) app(one project, app attach.App) {
	top := p.reserve(rowHeight)
	row := image.Rect(bleed, top, width-bleed, top+rowHeight)
	p.rows = append(p.rows, row)
	lit := p.at.In(row)
	if lit {
		p.round(image.Rect(row.Min.X, row.Min.Y+1, row.Max.X, row.Max.Y-1), 4, rowLit)
	}
	shape, tint := form(app)
	p.mark(glyphX, top+rowHeight/2, shape, tint)

	// What the shape already says is not written again: a ready app only has to say since when.
	said := app.Status
	switch {
	case app.Drift != "":
		said = app.Drift
	case app.Status == "idle" && app.Last != "":
		said = app.Last
	case app.Status == "ready" && !app.Since.IsZero():
		said = age(p.now.Sub(app.Since))
	}
	saidTint := faint
	if shape == cross {
		saidTint = brokenText
	}

	second := image.Rect(width-pad-buttonHeight, top+2, width-pad, top+2+buttonHeight)
	first := second.Sub(image.Pt(buttonHeight+4, 0))
	slots := []struct {
		box image.Rectangle
		do  deed
	}{{first, start}}
	if app.Status != "idle" {
		slots = []struct {
			box image.Rectangle
			do  deed
		}{{first, stop}, {second, restart}}
	}
	waiting := one.pending[app.Name].After(p.now)
	restFill, restTint := ground, faint
	if lit {
		restFill, restTint = key, ink
	}
	for _, slot := range slots {
		centerX, centerY := slot.box.Min.X+buttonHeight/2, slot.box.Min.Y+buttonHeight/2
		if waiting {
			p.pictogram(slot.do, centerX, centerY, asleep)
			continue
		}
		// The zone covers the row's full height: the pictogram looks small, and clicks large.
		z := zone{at: image.Rect(slot.box.Min.X-2, top, slot.box.Max.X+2, top+rowHeight),
			do: slot.do, config: one.config, name: app.Name}
		p.round(slot.box, 4, p.tone(z, restFill, keyLit, keyDown))
		p.pictogram(slot.do, centerX, centerY, restTint)
		p.zones = append(p.zones, z)
		if p.at.In(z.at) {
			said, saidTint = string(slot.do), ink // the state column says what the click will do
		}
	}
	if waiting {
		said, saidTint = "…", faint
	}

	// The name stops before the state column, which is at the same height: two texts on one row share
	// the width, they do not overlap.
	p.write(nameX, top+19, p.fitting(app.Name, statusX-8-nameX, p.with.text), p.with.text, ink)
	p.write(statusX, top+19, p.fitting(said, first.Min.X-8-statusX, p.with.text), p.with.text, saidTint)
	// Where an app listens when it starts without listening at its address does not fit in the state
	// column: it is a message, read in full under its row.
	p.note(nameX, app.Astray, faint)
}

// starting is what the row of a project whose session is coming up says: there is no button anymore,
// hence no second click possible, and the wait shows.
const starting = "starting…"

// actions writes a session's two gestures — stop its apps, or forget it: two intents, two buttons —
// and the way to its interface. A cold project has none: it has nothing to stop, and its gestures are
// on its title line.
func (p *painter) actions(one project) {
	top := p.reserve(actionRow) + (actionRow-buttonHeight)/2
	right := p.button(top, width-pad, "forget", forget, one.config, "")
	right = p.button(top, right, "stop apps", closeAll, one.config, "")
	label := "open the TUI"
	if watching(*one.live) {
		label = "raise the TUI"
	}
	p.button(top, right, label, openTUI, one.config, "")
}

// global writes the two gestures that apply to everything: stop the apps of every project, or go away
// — icon included. Two distinct intents, hence never the same button.
//
// The beta's gesture takes a row of its own below them, and the beta offered comes with the violet
// square of the icon it will bring.
func (p *painter) global(projects []project, news renewal) {
	newsTint := faint
	if news.failed {
		newsTint = brokenText
	}
	for _, line := range wrapped(news.said, width-2*pad, func(text string) int { return p.widthOf(text, p.with.text) }) {
		p.write(pad, p.reserve(newsRow)+15, line, p.with.text, newsTint)
	}
	top := p.reserve(actionRow) + (actionRow-buttonHeight)/2
	right := width - pad
	right = p.button(top, right, "quit "+build.Name(), quitDsvs, "", "")
	for _, one := range projects {
		if one.live != nil {
			right = p.button(top, right, "stop all apps", stopEverything, "", "")
			break
		}
	}
	switch news.stage {
	case found:
		p.button(top, right, "update to "+news.to, renew, "", "")
	case idle:
		p.button(top, right, "check for updates", look, "", "")
	}
	if news.stage == working || news.beta == nothing {
		return
	}
	top = p.reserve(actionRow) + (actionRow-buttonHeight)/2
	left := p.button(top, width-pad, string(news.beta), news.beta, "", "")
	if news.beta == installBeta {
		p.round(image.Rect(pad, top+7, pad+10, top+17), 2, branch)
		p.write(pad+16, top+17, p.fitting(build.Beta+" "+news.betaOut+" is out", left-pad-16, p.with.text), p.with.text, faint)
	}
}

// button lays a button whose top edge is at top and right edge at right, and returns the left edge of
// the next one. Its look depends only on the gesture it carries: primary for the update, discreet for
// those that forget or quit, the same everywhere else: buttons thus follow one another from right to
// left, without any of them having to know its neighbors' width. It never leaves the panel: the
// margin bounds its left edge, and its label is elided on the box that remains — a label that comes
// from elsewhere, like a version found, has no promised length.
func (p *painter) button(top, right int, label string, do deed, config, name string) int {
	labelWidth := p.widthOf(label, p.with.text)
	z := zone{at: image.Rect(max(pad, right-labelWidth-2*buttonPadX), top, right, top+buttonHeight), do: do, config: config, name: name}
	rest, lit, down, text := key, keyLit, keyDown, ink
	switch do {
	case renew:
		rest, lit, down, text = accent, accentLit, accentDown, white
	case forget, quitDsvs, removeBeta:
		if !p.at.In(z.at) {
			text = subdued
		}
	}
	p.round(z.at, 4, p.tone(z, rest, lit, down))
	p.write(z.at.Min.X+buttonPadX, top+17, p.fitting(label, z.at.Dx()-2*buttonPadX, p.with.text), p.with.text, text)
	p.zones = append(p.zones, z)
	return z.at.Min.X - buttonGap
}

// tone is a control's background: pressed while the gesture started here is held here, lit under the
// pointer, at rest otherwise. Dragged elsewhere, the background comes back.
func (p *painter) tone(z zone, rest, lit, down color.RGBA) color.RGBA {
	switch {
	case !p.at.In(z.at):
		return rest
	case p.pressed == z:
		return down
	}
	return lit
}

// line separates two sessions.
func (p *painter) line() {
	middle := p.reserve(ruleRow) + ruleRow/2
	draw.Draw(p.canvas, image.Rect(pad, middle, width-pad, middle+1), image.NewUniform(rule), image.Point{}, draw.Src)
}

func (p *painter) write(x, baseline int, text string, face font.Face, tint color.Color) {
	drawer := font.Drawer{Dst: p.canvas, Src: image.NewUniform(tint), Face: face,
		Dot: fixed.P(x, baseline)}
	drawer.DrawString(text)
}

func (p *painter) widthOf(text string, face font.Face) int {
	return font.MeasureString(face, text).Ceil()
}

// fitting cuts a text to the room it is given. A panel whose columns overlap cannot be read; an
// elided end is better. Every text the panel does not choose itself goes through here: a path, a
// host, an app name come from elsewhere and have no promised length.
func (p *painter) fitting(text string, room int, face font.Face) string {
	if p.widthOf(text, face) <= room {
		return text
	}
	runes := []rune(text)
	for len(runes) > 1 {
		runes = runes[:len(runes)-1]
		if short := string(runes) + "…"; p.widthOf(short, face) <= room {
			return short
		}
	}
	return ""
}

// wrapped cuts a text into lines that fit this width: at spaces while it has some, inside a word when
// there is no choice.
func wrapped(text string, room int, widthOf func(string) int) []string {
	var lines []string
	for {
		text = strings.TrimLeft(text, " ")
		if text == "" {
			return lines
		}
		if widthOf(text) <= room {
			return append(lines, text)
		}
		runes := []rune(text)
		for len(runes) > 1 && widthOf(string(runes)) > room {
			runes = runes[:len(runes)-1]
		}
		cut := len(string(runes))
		if space := strings.LastIndexByte(text[:cut], ' '); space > 0 {
			cut = space
		}
		lines = append(lines, text[:cut])
		text = text[cut:]
	}
}

// mark draws a state's shape: a filled disc, a ring, a half disc or a cross.
func (p *painter) mark(cx, cy int, shape glyph, tint color.Color) {
	const r = 5.5
	x, y := float32(cx), float32(cy)
	p.fill(image.Rect(cx-7, cy-7, cx+7, cy+7), tint, func(n pen) {
		switch shape {
		case filled:
			n.circle(x, y, r, solid)
		case half:
			n.wedge(x, y, r, math.Pi/2, 3*math.Pi/2)
			fallthrough
		case ring:
			n.circle(x, y, r, solid)
			n.circle(x, y, r-1.75, hollow)
		case cross:
			n.stroke(x-4.2, y-4.2, x+4.2, y+4.2, 2)
			n.stroke(x-4.2, y+4.2, x+4.2, y-4.2, 2)
		}
	})
}

// pictogram draws what a gesture does: a triangle to start, a square to stop, an arrowed ring to
// restart, a cross to close, a chevron to unfold. A drawn shape, not a font glyph — which the font
// might lack.
func (p *painter) pictogram(do deed, cx, cy int, tint color.Color) {
	x, y := float32(cx), float32(cy)
	p.fill(image.Rect(cx-8, cy-8, cx+8, cy+8), tint, func(n pen) {
		switch do {
		case start:
			n.polygon(x-3, y-5, x+5, y, x-3, y+5)
		case stop:
			n.roundBox(image.Rect(cx-4, cy-4, cx+4, cy+4), 1)
		case restart:
			n.loop(x, y)
		case dismiss:
			n.stroke(x-4.5, y-4.5, x+4.5, y+4.5, 1.6)
			n.stroke(x-4.5, y+4.5, x+4.5, y-4.5, 1.6)
		case unfold:
			n.polygon(x-2, y-4, x+2.5, y, x-2, y+4)
		case drop:
			n.polygon(x-3.5, y-2, x+3.5, y-2, x, y+2.5)
		case choose:
			n.stroke(x-4.5, y+0.5, x-1.5, y+3.5, 1.7)
			n.stroke(x-1.5, y+3.5, x+4.5, y-3.5, 1.7)
		}
	})
}

// round fills a box with rounded corners.
func (p *painter) round(box image.Rectangle, radius float32, tint color.Color) {
	p.fill(box, tint, func(n pen) { n.roundBox(box, radius) })
}

// fill paints into this box the outlines the drawing traces, anti-aliased. The raster is only the
// size of the box: a rasterizer over the whole canvas would reset on every shape.
func (p *painter) fill(box image.Rectangle, tint color.Color, trace func(pen)) {
	z := vector.NewRasterizer(box.Dx(), box.Dy())
	trace(pen{z: z, dx: float32(box.Min.X), dy: float32(box.Min.Y)})
	z.Draw(p.canvas, box, image.NewUniform(tint), image.Point{})
}

// pen traces outlines in canvas coordinates into a box's raster. Outlines fill by winding: a circle
// traced backwards makes a hole in the one around it.
type pen struct {
	z      *vector.Rasterizer
	dx, dy float32
}

// solid and hollow are a circle's direction: filled, or carved out of what surrounds it.
const (
	solid  = 1
	hollow = -1
)

// kappa approximates a quarter circle with a cubic Bezier curve.
const kappa = 0.5523

func (n pen) move(x, y float32) { n.z.MoveTo(x-n.dx, y-n.dy) }

func (n pen) line(x, y float32) { n.z.LineTo(x-n.dx, y-n.dy) }

func (n pen) curve(x1, y1, x2, y2, x, y float32) {
	n.z.CubeTo(x1-n.dx, y1-n.dy, x2-n.dx, y2-n.dy, x-n.dx, y-n.dy)
}

func (n pen) polygon(points ...float32) {
	n.move(points[0], points[1])
	for i := 2; i < len(points); i += 2 {
		n.line(points[i], points[i+1])
	}
	n.z.ClosePath()
}

func (n pen) roundBox(box image.Rectangle, radius float32) {
	x0, y0, x1, y1 := float32(box.Min.X), float32(box.Min.Y), float32(box.Max.X), float32(box.Max.Y)
	c := radius * (1 - kappa)
	n.move(x0+radius, y0)
	n.line(x1-radius, y0)
	n.curve(x1-c, y0, x1, y0+c, x1, y0+radius)
	n.line(x1, y1-radius)
	n.curve(x1, y1-c, x1-c, y1, x1-radius, y1)
	n.line(x0+radius, y1)
	n.curve(x0+c, y1, x0, y1-c, x0, y1-radius)
	n.line(x0, y0+radius)
	n.curve(x0, y0+c, x0+c, y0, x0+radius, y0)
	n.z.ClosePath()
}

func (n pen) circle(cx, cy, r, turn float32) {
	n.move(cx+r, cy)
	n.curve(cx+r, cy+turn*r*kappa, cx+r*kappa, cy+turn*r, cx, cy+turn*r)
	n.curve(cx-r*kappa, cy+turn*r, cx-r, cy+turn*r*kappa, cx-r, cy)
	n.curve(cx-r, cy-turn*r*kappa, cx-r*kappa, cy-turn*r, cx, cy-turn*r)
	n.curve(cx+r*kappa, cy-turn*r, cx+r, cy-turn*r*kappa, cx+r, cy)
	n.z.ClosePath()
}

// wedge is a sector of a disc, from one angle to the other, clockwise on screen.
func (n pen) wedge(cx, cy, r float32, from, to float64) {
	const steps = 24
	n.move(cx, cy)
	for i := range steps + 1 {
		angle := from + (to-from)*float64(i)/steps
		n.line(cx+r*float32(math.Cos(angle)), cy+r*float32(math.Sin(angle)))
	}
	n.z.ClosePath()
}

// stroke is a thick line, traced as a quadrilateral.
func (n pen) stroke(x0, y0, x1, y1, thickness float32) {
	dx, dy := x1-x0, y1-y0
	length := float32(math.Hypot(float64(dx), float64(dy)))
	nx, ny := -dy/length*thickness/2, dx/length*thickness/2
	n.polygon(x0+nx, y0+ny, x1+nx, y1+ny, x1-nx, y1-ny, x0-nx, y0-ny)
}

// loop is the restart's arrowed ring: a ring open towards noon, whose tip turns clockwise.
func (n pen) loop(cx, cy float32) {
	const r, thickness, steps = 5.0, 1.7, 32
	from, to := -math.Pi/2+0.95, 3*math.Pi/2-0.2
	at := func(i int, radius float32) (float32, float32) {
		angle := from + (to-from)*float64(i)/steps
		return cx + radius*float32(math.Cos(angle)), cy + radius*float32(math.Sin(angle))
	}
	n.move(at(0, r))
	for i := 1; i <= steps; i++ {
		n.line(at(i, r))
	}
	for i := steps; i >= 0; i-- {
		n.line(at(i, r-thickness))
	}
	n.z.ClosePath()

	tipX, tipY := at(steps, r-thickness/2)
	tangentX, tangentY := float32(-math.Sin(to)), float32(math.Cos(to))
	normalX, normalY := float32(math.Cos(to)), float32(math.Sin(to))
	n.polygon(tipX+tangentX*3.4, tipY+tangentY*3.4,
		tipX-tangentX*0.6+normalX*3, tipY-tangentY*0.6+normalY*3,
		tipX-tangentX*0.6-normalX*3, tipY-tangentY*0.6-normalY*3)
}

type glyph int

const (
	ring glyph = iota
	filled
	half
	cross
)

// form gives an app's shape and color: what it is doing, and whether its last run ended badly.
func form(app attach.App) (glyph, color.Color) {
	switch {
	// The status, not Ready: a stopping app still answers until its process dies, and its stop has to
	// show from the click on.
	case app.Status == "ready":
		return filled, ready
	case app.Status != "idle":
		return half, waking
	case app.Fell:
		return cross, broken
	}
	return ring, asleep
}

// Mood is what all the sessions say together: what the status bar icon shows, from calmest to most
// serious.
type Mood int

const (
	// Idle: no app is running.
	Idle Mood = iota
	// Running: everything that should run is running.
	Running
	// Starting: at least one app is coming up, or stopping.
	Starting
	// Failed: at least one app fell.
	Failed
)

// Overall is the single rule for what the sessions say together: the worst shape any app of any of
// them has, the very one the panel draws in front of its name.
func Overall(states []attach.State) Mood {
	mood := Idle
	for _, state := range states {
		for _, app := range state.Apps {
			switch shape, _ := form(app); shape {
			case cross:
				return Failed
			case half:
				mood = max(mood, Starting)
			case filled:
				mood = max(mood, Running)
			}
		}
	}
	return mood
}

// watching says whether an interface is already watching this session: there is no offer to open a
// second one when bringing back the existing one is enough.
func watching(state attach.State) bool {
	for _, seen := range state.Interfaces {
		if seen.Kind == "tui" {
			return true
		}
	}
	return false
}

// url writes a facade as one types it in a browser. The gateway serves in clear text on the session's
// port, and a port that is the scheme's own is not typed.
func url(host string, port int) string {
	if port == 80 {
		return "http://" + host
	}
	return "http://" + host + ":" + strconv.Itoa(port)
}

// labels names the sessions in the shortest way that tells them apart. The configuration's folder is
// usually enough; two worktrees of the same project carry the same one, and then go up one level —
// that is where what separates them reads.
func labels(projects []project) map[string]string {
	dirs := make([]string, 0, len(projects))
	for _, one := range projects {
		dirs = append(dirs, filepath.Dir(one.config))
	}
	for depth := 1; depth <= 4; depth++ {
		named, seen := make(map[string]string, len(projects)), make(map[string]bool, len(projects))
		for i, one := range projects {
			label := suffix(dirs[i], depth)
			named[one.config], seen[label] = label, true
		}
		if len(seen) == len(projects) {
			return named
		}
	}
	named := make(map[string]string, len(projects))
	for i, one := range projects {
		named[one.config] = dirs[i]
	}
	return named
}

// suffix returns the last segments of a path.
func suffix(dir string, depth int) string {
	segments := strings.Split(filepath.Clean(dir), string(filepath.Separator))
	if len(segments) > depth {
		segments = segments[len(segments)-depth:]
	}
	return filepath.Join(segments...)
}

// age writes a duration as it reads at a glance.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dmin", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02d", int(d.Hours()), int(d.Minutes())%60)
	}
}
