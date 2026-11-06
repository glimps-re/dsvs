package panel

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"

	"dsvs/internal/attach"
	"dsvs/internal/build"
)

// The panel's measures, in pixels. A single column, fixed-height rows: what decides the size is the
// number of apps.
const (
	width      = 420
	pad        = 12
	rowHeight  = 24
	headerRow  = 26
	titleRow   = 30
	actionRow  = 30
	facadeRow  = 20
	ruleRow    = 12
	buttonPadX = 7
	statusX    = pad + 130
)

var (
	ground = color.RGBA{0x1c, 0x1d, 0x22, 0xff}
	ink    = color.RGBA{0xe8, 0xe8, 0xec, 0xff}
	faint  = color.RGBA{0x8b, 0x8d, 0x98, 0xff}
	rule   = color.RGBA{0x2e, 0x30, 0x38, 0xff}
	key    = color.RGBA{0x33, 0x36, 0x40, 0xff}
	keyLit = color.RGBA{0x46, 0x4a, 0x59, 0xff}
	rowLit = color.RGBA{0x28, 0x2a, 0x33, 0xff}
	ready  = color.RGBA{0x6f, 0xc2, 0x76, 0xff}
	waking = color.RGBA{0xe0, 0xb3, 0x4a, 0xff}
	broken = color.RGBA{0xe0, 0x6c, 0x6c, 0xff}
	asleep = color.RGBA{0x5c, 0x5f, 0x6b, 0xff}
)

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
	// project already seen, forget forgets it without touching what runs, stopEverything stops the apps
	// of every project, and quitDsvs goes away with the icon.
	openTUI        deed = "open the tui"
	open           deed = "wake"
	forget         deed = "forget"
	stopEverything deed = "stop everything"
	quitDsvs       deed = "quit dsvs"

	// browse opens a facade in the default browser.
	browse deed = "browse"

	// look searches for a new version of the binary, renew takes it.
	look  deed = "look"
	renew deed = "renew"
)

// project is a panel row: a project already seen, what of it runs when it runs, and what a gesture
// gave when it failed — the reason reads where the gesture took place.
type project struct {
	config string
	live   *attach.State
	failed string
	// waking says its session is coming up: there is then nothing to click on its row, and that is
	// what the row says.
	waking bool
}

// renewal is what the panel knows of the update. Three states and not one more: it is working and
// shows its step, it found a version and offers to take it, or it has something to say and offers to
// search again. The working state is built by doing, which requires its step: that is what rules out
// a panel with nothing to show or to click.
type renewal struct {
	stage stage
	said  string
	// to is the version found, and only means something under found.
	to string
}

// stage is where the update stands. Zero is the working state: nothing to click, and said tells the
// step.
type stage int

const (
	working stage = iota
	found
	idle
)

// doing is the update at work: it says where it stands, and there is nothing to click while it is.
func doing(said string) renewal { return renewal{stage: working, said: said} }

// zone is a region of the panel and what a click does there. config names the session concerned, name
// what the order designates in it: the app for an app order, the URL for browse.
type zone struct {
	at     image.Rectangle
	do     deed
	config string
	name   string
}

// faces are the panel's two fonts: the text, and the titles.
type faces struct {
	text font.Face
	bold font.Face
}

// paint draws the sessions and returns the image to push, the zones to click, and the app rows as it
// laid them out. at is the pointer: what is under it lights up, so that one sees what one is about to
// click — and the drawing says where its rows are, so the window does not have to redo its geometry to
// know when the hover changed. Nothing X11 here.
func paint(projects []project, news renewal, with faces, now time.Time, at image.Point) (*image.RGBA, []zone, []image.Rectangle) {
	p := &painter{with: with, now: now, at: at}
	p.stretch(pad + headerRow + actionRow + pad)

	p.reserve(pad)
	p.header()
	if len(projects) == 0 {
		p.write(pad, p.reserve(rowHeight)+15, "no project yet", with.text, faint)
	}
	named := labels(projects)
	for i, one := range projects {
		if i > 0 {
			p.line()
		}
		p.project(one, named[one.config])
	}
	p.line()
	p.global(projects, news)
	p.reserve(pad)
	return p.cropped(), p.zones, p.rows
}

// painter carries the drawing in progress: the canvas, the line being written, and the zones already
// laid out.
type painter struct {
	canvas *image.RGBA
	with   faces
	now    time.Time
	// at is the pointer, outside the image when it is elsewhere.
	at    image.Point
	y     int
	zones []zone
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
	p.write(pad, top+16, build.Label(), p.with.bold, faint)
	box := image.Rect(width-pad-18, top+3, width-pad, top+21)
	if p.at.In(box) {
		draw.Draw(p.canvas, box, image.NewUniform(keyLit), image.Point{}, draw.Src)
	}
	p.cross(box.Inset(5), ink)
	p.zones = append(p.zones, zone{at: box, do: dismiss})
}

// cross draws the close cross with strokes: nothing to ask of a font, which might not have the
// glyph.
func (p *painter) cross(inner image.Rectangle, tint color.Color) {
	side := min(inner.Dx(), inner.Dy())
	for i := range side {
		for thick := range 2 {
			p.canvas.Set(inner.Min.X+i+thick, inner.Min.Y+i, tint)
			p.canvas.Set(inner.Min.X+side-1-i+thick, inner.Min.Y+i, tint)
		}
	}
}

// project writes a project: its title, what of it runs, and what can be done to it. Cold, there is
// only its path and a way to start it — that is what remains of a project between two sessions.
func (p *painter) project(one project, named string) {
	top := p.reserve(titleRow)
	right := "not running"
	if one.live != nil {
		right = ":" + strconv.Itoa(one.live.Port)
	}
	at := width - pad - p.widthOf(right, p.with.text)
	p.write(pad, top+15, p.fitting(named, at-8-pad, p.with.bold), p.with.bold, ink)
	p.write(at, top+15, right, p.with.text, faint)

	// A failure reason comes from elsewhere and has no promised length: it takes the lines it needs
	// rather than ending in an ellipsis, the panel having no height decided in advance.
	for _, line := range wrapped(one.failed, width-2*pad-18, func(text string) int { return p.widthOf(text, p.with.text) }) {
		p.write(pad+18, p.reserve(rowHeight)+16, line, p.with.text, broken)
	}
	if one.live != nil {
		for _, app := range one.live.Apps {
			p.app(one.config, app)
		}
		p.facades(one.config, one.live.Facades, one.live.Port)
	}
	p.actions(one)
}

// facades writes the hosts this session serves, one by one and clickable: they are the links one
// opens, and a host cut in the middle can be neither read nor typed. So they wrap to the next line
// rather than being elided, like the failure reason above — the panel has no height set in advance.
// Only a host too long for a whole line ends in an ellipsis.
func (p *painter) facades(config string, facades []attach.Facade, port int) {
	const left = pad + 18
	right := width - pad
	// x at the right edge: the first host wraps there, and with no facade no line is taken.
	top, x := 0, right
	for _, facade := range facades {
		host := p.fitting(facade.Host, right-left-2*buttonPadX, p.with.text)
		wide := p.widthOf(host, p.with.text) + 2*buttonPadX
		if x+wide > right {
			top, x = p.reserve(facadeRow), left
		}
		box := image.Rect(x, top+1, x+wide, top+facadeRow-1)
		tint := faint
		if p.at.In(box) {
			draw.Draw(p.canvas, box, image.NewUniform(keyLit), image.Point{}, draw.Src)
			tint = ink
		}
		p.write(x+buttonPadX, top+14, host, p.with.text, tint)
		draw.Draw(p.canvas, image.Rect(x+buttonPadX, top+16, box.Max.X-buttonPadX, top+17),
			image.NewUniform(tint), image.Point{}, draw.Src)
		p.zones = append(p.zones, zone{at: box, do: browse, config: config, name: url(facade.Host, port)})
		x = box.Max.X
	}
}

// app writes an app row: its shape, its name, what it is doing, and the buttons that drive it. The
// shape says the state without depending on a glyph a font might lack.
func (p *painter) app(config string, app attach.App) {
	top := p.reserve(rowHeight)
	row := image.Rect(pad-4, top, width-pad+4, top+rowHeight)
	p.rows = append(p.rows, row)
	if p.at.In(row) {
		draw.Draw(p.canvas, row, image.NewUniform(rowLit), image.Point{}, draw.Src)
	}
	shape, tint := form(app)
	p.mark(pad+7, top+rowHeight/2, shape, tint)
	// The name stops before the state column, which is at the same height: two texts on one row share
	// the width, they do not overlap.
	p.write(pad+18, top+16, p.fitting(app.Name, statusX-8-pad-18, p.with.text), p.with.text, ink)

	// The buttons first: what can be done depends on what the app is doing, and they decide the room
	// left to the text — otherwise it would run under them.
	right := width - pad
	if app.Status == "idle" {
		right = p.button(top, right, "start", start, config, app.Name)
	} else {
		right = p.button(top, right, "restart", restart, config, app.Name)
		right = p.button(top, right, "stop", stop, config, app.Name)
	}

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
	p.write(statusX, top+16, p.fitting(said, right-8-statusX, p.with.text), p.with.text, faint)
}

// starting is what the row of a project whose session is coming up says: there is no button anymore,
// hence no second click possible, and the wait shows.
const starting = "starting…"

// actions writes a project's two gestures — stop its apps, or forget it: two intents, two buttons —
// and the way to its interface. Cold, there is nothing to stop: starting takes its place. A project
// whose session is coming up offers nothing at all: waking and forgetting are incompatible on the same
// row, and forgetting would take away the one where the link was about to show.
func (p *painter) actions(one project) {
	top := p.reserve(actionRow)
	right := width - pad
	if one.waking {
		p.write(right-p.widthOf(starting, p.with.text), top+16, starting, p.with.text, faint)
		return
	}
	right = p.button(top, right, "forget", forget, one.config, "")
	if one.live == nil {
		p.button(top, right, "start", open, one.config, "")
		return
	}
	right = p.button(top, right, "stop apps", closeAll, one.config, "")
	label := "open the TUI"
	if watching(*one.live) {
		label = "raise the TUI"
	}
	p.button(top, right, label, openTUI, one.config, "")
}

// global writes the two gestures that apply to everything: stop the apps of every project, or go away
// — icon included. Two distinct intents, hence never the same button.
func (p *painter) global(projects []project, news renewal) {
	for _, line := range wrapped(news.said, width-2*pad, func(text string) int { return p.widthOf(text, p.with.text) }) {
		p.write(pad, p.reserve(rowHeight)+16, line, p.with.text, faint)
	}
	top := p.reserve(actionRow)
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
}

// button lays a button whose right edge is at right, and returns the left edge: buttons thus follow
// one another from right to left, without any of them having to know its neighbors' width. It never
// leaves the panel: the margin bounds its left edge, and its label is elided on the box that remains —
// a label that comes from elsewhere, like a version found, has no promised length.
func (p *painter) button(top, right int, label string, do deed, config, name string) int {
	text := p.widthOf(label, p.with.text)
	box := image.Rect(max(pad, right-text-2*buttonPadX), top+3, right, top+rowHeight-3)
	fill := key
	if p.at.In(box) {
		fill = keyLit
	}
	draw.Draw(p.canvas, box, image.NewUniform(fill), image.Point{}, draw.Src)
	p.write(box.Min.X+buttonPadX, top+16, p.fitting(label, box.Dx()-2*buttonPadX, p.with.text), p.with.text, ink)
	p.zones = append(p.zones, zone{at: box, do: do, config: config, name: name})
	return box.Min.X - 6
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
	const r = 5
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			d := dx*dx + dy*dy
			paint := false
			switch shape {
			case filled:
				paint = d <= r*r
			case ring:
				paint = d <= r*r && d >= (r-2)*(r-2)
			case half:
				paint = d <= r*r && (dx <= 0 || d >= (r-2)*(r-2))
			case cross:
				paint = abs(dx-dy) <= 1 || abs(dx+dy) <= 1
			}
			if paint {
				p.canvas.Set(cx+dx, cy+dy, tint)
			}
		}
	}
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
	case app.Last != "" && app.Last != "stopped":
		return cross, broken
	}
	return ring, asleep
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

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
