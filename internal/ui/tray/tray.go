package tray

import (
	"errors"
	"image"
	"image/color"
	"image/draw"
	"io"
	"log"
	"math"
	"os"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"golang.org/x/image/vector"

	"dsvs/internal/build"
	"dsvs/internal/ui/panel"
)

const (
	// side is the size we ask for. The bar may impose another one, and we follow.
	side = 22
	// requestDock is the opcode that asks for a place in the bar.
	requestDock = 0
	// mapped is the _XEMBED_INFO flag saying our window wants to be shown, and 1 the protocol version we
	// speak — version 0 is accepted and never shows anything.
	mapped  = 1
	version = 1
)

// Available says whether there is a status bar here. It is the question to ask before promising an
// icon: with no owner of the selection, nobody would show it.
func Available() bool {
	quiet()
	conn, err := xgb.NewConn()
	if err != nil {
		return false
	}
	defer conn.Close()
	return bar(conn) != 0
}

// opener opens the panel against this screen rectangle, reports when its window leaves, and returns
// only afterwards: what a panel started — an update — outlives it as long as needed.
type opener func(anchor image.Rectangle, until <-chan struct{}, gone func()) error

// Run places the icon and returns only when the X session ends. open receives the icon's rectangle in
// screen coordinates — the panel decides which side to open on, the icon knows nothing of the screen —
// and a channel whose closing closes it: the second click on the icon removes what the first one
// placed.
func Run(open opener) error {
	quiet()
	conn, err := xgb.NewConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	screen := xproto.Setup(conn).DefaultScreen(conn)
	if bar(conn) == 0 {
		return errors.New("no status bar on this display")
	}

	icon := &icon{conn: conn, screen: screen, size: side,
		msb: xproto.Setup(conn).ImageByteOrder == xproto.ImageOrderMSBFirst}
	if err := icon.show(); err != nil {
		return err
	}
	// The status bar may restart: it says so by taking the selection back, and that is when to ask for
	// a place again. Without it the icon vanishes until the next launch.
	xproto.ChangeWindowAttributes(conn, screen.Root, xproto.CwEventMask,
		[]uint32{xproto.EventMaskStructureNotify})
	icon.dock()
	done := make(chan struct{})
	defer close(done)
	go follow(done, icon.say)
	return icon.live(open)
}

type icon struct {
	conn   *xgb.Conn
	screen *xproto.ScreenInfo
	win    xproto.Window
	gc     xproto.Gcontext
	size   int
	msb    bool
	// shown, when non-nil, is the open panel: closing it closes the panel. It is also what says there is
	// one, and that a second click must remove it rather than place another.
	//
	// aiming is the click in progress, or nil: what to do if the button is released over the icon.
	//
	// mood is what the icon says, frame the image of the spinning badge. They change from goroutines
	// other than the event loop, and mu also guards the size.
	mu     sync.Mutex
	shown  chan struct{}
	aiming *aim
	mood   panel.Mood
	frame  int
	// spinning stops the badge's timer, and is non-nil only while mood spins.
	spinning chan struct{}
}

// aim is a click started on the icon. Its decision is taken on press, while we still know whether a
// panel was there — by release it may already have vanished, and we would place another. at is the
// icon itself, in root coordinates: the anchor the panel will sit against.
type aim struct {
	at   image.Rectangle
	open bool
}

// show creates the icon's window with everything the protocol asks for.
func (i *icon) show() error {
	win, err := xproto.NewWindowId(i.conn)
	if err != nil {
		return err
	}
	i.win = win
	if err := xproto.CreateWindowChecked(i.conn, i.screen.RootDepth, win, i.screen.Root,
		0, 0, uint16(i.size), uint16(i.size), 0,
		xproto.WindowClassInputOutput, i.screen.RootVisual,
		xproto.CwBackPixel|xproto.CwEventMask,
		[]uint32{0x000000,
			xproto.EventMaskExposure | xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease |
				xproto.EventMaskStructureNotify,
		}).Check(); err != nil {
		return err
	}
	i.describe()

	gc, err := xproto.NewGcontextId(i.conn)
	if err != nil {
		return err
	}
	i.gc = gc
	return xproto.CreateGCChecked(i.conn, gc, xproto.Drawable(win), 0, nil).Check()
}

// describe sets the protocol's properties. Every one of them was needed: without them, the icon is
// embedded on the VM side and never relayed to the screen (measured against a GTK control icon).
func (i *icon) describe() {
	host, _ := os.Hostname()
	label := build.Label()
	for _, said := range []struct{ property, kind, text string }{
		// The window's name is what the status bar shows on hover: the bar tint says which binary it
		// is, the tooltip adds its version.
		{"WM_NAME", "STRING", label},
		{"_NET_WM_NAME", "UTF8_STRING", label},
		{"WM_ICON_NAME", "STRING", label},
		{"WM_CLIENT_MACHINE", "STRING", host},
	} {
		xproto.ChangeProperty(i.conn, xproto.PropModeReplace, i.win, i.atom(said.property),
			i.atom(said.kind), 8, uint32(len(said.text)), []byte(said.text))
	}
	class := append([]byte("dsvs\x00"), []byte("Dsvs\x00")...)
	xproto.ChangeProperty(i.conn, xproto.PropModeReplace, i.win, i.atom("WM_CLASS"),
		i.atom("STRING"), 8, uint32(len(class)), class)

	i.cards("_XEMBED_INFO", "_XEMBED_INFO", version, mapped)
	i.cards("_NET_WM_PID", "CARDINAL", uint32(os.Getpid()))
	// PMinSize|PMaxSize|PBaseSize, then the sizes: nothing else tells the bar how much room to give
	// us, and it does not guess.
	i.cards("WM_NORMAL_HINTS", "WM_SIZE_HINTS", 16|32|256,
		0, 0, 0, 0, side, side, side, side, 0, 0, 0, 0, 0, 0, side, side, 0)
}

// dock asks for our place in the bar.
func (i *icon) dock() {
	owner := bar(i.conn)
	if owner == 0 {
		return
	}
	message := xproto.ClientMessageEvent{Format: 32, Window: owner, Type: i.atom("_NET_SYSTEM_TRAY_OPCODE"),
		Data: xproto.ClientMessageDataUnionData32New([]uint32{
			uint32(time.Now().UnixMilli()), requestDock, uint32(i.win), 0, 0})}
	xproto.SendEvent(i.conn, false, owner, xproto.EventMaskNoEvent, string(message.Bytes()))
}

// live keeps the icon alive: it repaints when asked to, and a click opens the panel against the icon.
func (i *icon) live(open opener) error {
	for {
		event, err := i.conn.WaitForEvent()
		if event == nil && err == nil {
			return nil // the display went away
		}
		switch e := event.(type) {
		case xproto.ExposeEvent:
			i.draw()
		case xproto.ConfigureNotifyEvent:
			if e.Window == i.win && int(e.Width) > 0 {
				i.mu.Lock()
				i.size = min(int(e.Width), int(e.Height))
				i.mu.Unlock()
				i.draw()
			}
		case xproto.ButtonPressEvent:
			i.press(image.Pt(int(e.RootX), int(e.RootY)), image.Pt(int(e.EventX), int(e.EventY)))
		case xproto.ButtonReleaseEvent:
			i.release(open, image.Pt(int(e.EventX), int(e.EventY)))
		case xproto.ClientMessageEvent:
			if e.Type == i.atom("MANAGER") {
				i.dock()
			}
		}
	}
}

// press starts a click. Nothing moves yet: a click is a round trip on the same element, and it acts
// on release — like everywhere else, and a window manager that grabs the press to give focus no longer
// steals our gesture on the way. root and where are the same pointer said twice — in root coordinates
// and relative to the icon: their difference is the icon's origin, the only place we learn where it
// is.
func (i *icon) press(root, where image.Point) {
	i.mu.Lock()
	defer i.mu.Unlock()
	origin := root.Sub(where)
	i.aiming = &aim{at: image.Rectangle{Min: origin, Max: origin.Add(image.Pt(i.size, i.size))},
		open: i.shown == nil}
}

// release ends the click: it opens the panel against the icon, or removes the one already there.
// Released anywhere but on the icon, the gesture is abandoned.
func (i *icon) release(open opener, where image.Point) {
	i.mu.Lock()
	aiming := i.aiming
	i.aiming = nil
	if aiming == nil || !where.In(image.Rect(0, 0, i.size, i.size)) {
		i.mu.Unlock()
		return
	}
	if !aiming.open {
		shown := i.shown
		i.shown = nil
		i.mu.Unlock()
		if shown != nil {
			close(shown)
		}
		return
	}
	until := make(chan struct{})
	i.shown = until
	i.mu.Unlock()

	go func() { _ = open(aiming.at, until, func() { i.forget(until) }) }()
}

// forget drops this panel if it is still the one shown. Closed on its own — its cross, escape — the
// icon must know right away, or the next click would try to close what is no longer there, and be
// lost.
func (i *icon) forget(until chan struct{}) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.shown == until {
		i.shown = nil
	}
}

// say changes what the icon says, and repaints it. The badge's timer exists only while the icon has
// a spinning badge: it is born with the state that needs it and stops as soon as that state is left.
func (i *icon) say(mood panel.Mood) {
	i.mu.Lock()
	i.mood, i.frame = mood, 0
	switch {
	case turns(mood) && i.spinning == nil:
		i.spinning = make(chan struct{})
		go i.spin(i.spinning)
	case !turns(mood) && i.spinning != nil:
		close(i.spinning)
		i.spinning = nil
	}
	i.mu.Unlock()
	i.draw()
}

// turns says whether this state's badge spins: "starting" only, where it tells a startup from a
// hang. Elsewhere, nothing repaints unless a state changed.
func turns(mood panel.Mood) bool { return mood == panel.Starting }

// spin turns the badge a quarter turn every 500 ms, two 2 KB images per second, until stop closes.
func (i *icon) spin(stop <-chan struct{}) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			i.mu.Lock()
			i.frame++
			i.mu.Unlock()
			i.draw()
		}
	}
}

// draw pushes the icon's image. Every pixel is painted — relying on the parent's background made the
// icon uniformly white (measured), as the status bar gives us no background to inherit.
func (i *icon) draw() {
	i.mu.Lock()
	defer i.mu.Unlock()
	img := render(i.size, i.mood, build.Name(), i.frame)
	data := make([]byte, len(img.Pix))
	for x := 0; x < len(data); x += 4 {
		r, g, b := img.Pix[x], img.Pix[x+1], img.Pix[x+2]
		if i.msb {
			data[x+1], data[x+2], data[x+3] = r, g, b
			continue
		}
		data[x], data[x+1], data[x+2] = b, g, r
	}
	xproto.PutImage(i.conn, xproto.ImageFormatZPixmap, xproto.Drawable(i.win), i.gc,
		uint16(i.size), uint16(i.size), 0, 0, 0, i.screen.RootDepth, data)
}

var (
	tile   = rgb(0x101014)
	amber  = rgb(0xe0b34a)
	green  = rgb(0x6fc276)
	red    = rgb(0xe06c6c)
	violet = rgb(0xb9a3ff)
)

// render draws the icon: three bars on a dark tile, like a list of apps, and a badge in the corner as
// soon as something runs. What must stay readable is the silhouette, and a luminance gap is what
// survives whatever the status bar does to our pixels: the bar tint only says which binary this is,
// their luminance whether something runs, and the badge's shape — full disc, spinning ring or
// cross — how it runs.
func render(size int, mood panel.Mood, name string, frame int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), image.NewUniform(tile), image.Point{}, draw.Src)
	for _, bar := range bars(size) {
		draw.Draw(img, bar, image.NewUniform(tint(name, mood != panel.Idle)), image.Point{}, draw.Src)
	}
	if mood == panel.Idle {
		return img
	}

	diameter := float32((size + 1) / 2)
	halo := float32(1)
	if size >= 22 {
		halo = 2
	}
	center := float32(size) - 1 - diameter/2
	paint(img, tile, func(z *vector.Rasterizer) { disc(z, center, center, diameter/2+halo, solid) })
	switch mood {
	case panel.Running:
		paint(img, green, func(z *vector.Rasterizer) { disc(z, center, center, diameter/2, solid) })
	case panel.Starting:
		inner := diameter/2 - max(1.5, diameter/7)
		turn := float64(frame%4) * math.Pi / 2
		paint(img, amber, func(z *vector.Rasterizer) {
			disc(z, center, center, diameter/2, solid)
			disc(z, center, center, inner, hollow)
			half(z, center, center, diameter/2, turn-math.Pi/2, turn+math.Pi/2)
		})
	case panel.Failed:
		paint(img, red, func(z *vector.Rasterizer) { disc(z, center, center, diameter/2, solid) })
		arm, thickness := diameter*0.24, max(1.4, diameter/7)
		paint(img, color.White, func(z *vector.Rasterizer) {
			stroke(z, center-arm, center-arm, center+arm, center+arm, thickness)
			stroke(z, center-arm, center+arm, center+arm, center-arm, thickness)
		})
	}
	return img
}

// bars returns the three bars at the size we were given: the status bar decides, not us.
func bars(size int) []image.Rectangle {
	thick := max(2, size/7)
	margin := max(2, size/6)
	long := size - 2*margin
	short := max(2, long*3/5)
	gap := (size - 3*thick) / 4
	out := make([]image.Rectangle, 0, 3)
	for row, width := range []int{long, short, long} {
		top := gap + row*(thick+gap)
		out = append(out, image.Rect(margin, top, margin+width, top+thick))
	}
	return out
}

// tint is the bars' color: the working dsvs has them white, any other binary has them violet, and two
// icons in the same status bar tell apart without hovering. Unlit, they keep their tint and lose their
// luminance.
func tint(name string, lit bool) color.RGBA {
	switch {
	case name == build.Stable && lit:
		return rgb(0xf0f0f0)
	case name == build.Stable:
		return rgb(0x6b6e79)
	case lit:
		return violet
	}
	return rgb(0x6a5f93)
}

func rgb(hex uint32) color.RGBA {
	return color.RGBA{uint8(hex >> 16), uint8(hex >> 8), uint8(hex), 0xff}
}

// paint fills what the trace draws with this color, anti-aliased.
func paint(img *image.RGBA, tint color.Color, trace func(*vector.Rasterizer)) {
	z := vector.NewRasterizer(img.Bounds().Dx(), img.Bounds().Dy())
	trace(z)
	z.Draw(img, img.Bounds(), image.NewUniform(tint), image.Point{})
}

// solid and hollow are a disc's direction: filled, or carved out of what surrounds it.
const (
	solid  = 1
	hollow = -1
)

// kappa approximates a quarter circle with a cubic Bezier curve.
const kappa = 0.5523

func disc(z *vector.Rasterizer, cx, cy, r, turn float32) {
	z.MoveTo(cx+r, cy)
	z.CubeTo(cx+r, cy+turn*r*kappa, cx+r*kappa, cy+turn*r, cx, cy+turn*r)
	z.CubeTo(cx-r*kappa, cy+turn*r, cx-r, cy+turn*r*kappa, cx-r, cy)
	z.CubeTo(cx-r, cy-turn*r*kappa, cx-r*kappa, cy-turn*r, cx, cy-turn*r)
	z.CubeTo(cx+r*kappa, cy-turn*r, cx+r, cy-turn*r*kappa, cx+r, cy)
	z.ClosePath()
}

// half is a disc sector, from one angle to the other, clockwise on screen.
func half(z *vector.Rasterizer, cx, cy, r float32, from, to float64) {
	const steps = 24
	z.MoveTo(cx, cy)
	for i := range steps + 1 {
		angle := from + (to-from)*float64(i)/steps
		z.LineTo(cx+r*float32(math.Cos(angle)), cy+r*float32(math.Sin(angle)))
	}
	z.ClosePath()
}

// stroke is a thick line, traced as a quadrilateral.
func stroke(z *vector.Rasterizer, x0, y0, x1, y1, thickness float32) {
	dx, dy := x1-x0, y1-y0
	length := float32(math.Hypot(float64(dx), float64(dy)))
	nx, ny := -dy/length*thickness/2, dx/length*thickness/2
	z.MoveTo(x0+nx, y0+ny)
	z.LineTo(x1+nx, y1+ny)
	z.LineTo(x1-nx, y1-ny)
	z.LineTo(x0-nx, y0-ny)
	z.ClosePath()
}

// bar is the status bar's window, or zero: it holds the protocol's selection, and it is the one asked
// for a place.
func bar(conn *xgb.Conn) xproto.Window {
	selection, err := xproto.InternAtom(conn, false, uint16(len("_NET_SYSTEM_TRAY_S0")), "_NET_SYSTEM_TRAY_S0").Reply()
	if err != nil {
		return 0
	}
	held, err := xproto.GetSelectionOwner(conn, selection.Atom).Reply()
	if err != nil {
		return 0
	}
	return held.Owner
}

func (i *icon) cards(property, kind string, values ...uint32) {
	data := make([]byte, 4*len(values))
	for at, value := range values {
		xgb.Put32(data[4*at:], value)
	}
	xproto.ChangeProperty(i.conn, xproto.PropModeReplace, i.win, i.atom(property), i.atom(kind), 32,
		uint32(len(values)), data)
}

func (i *icon) atom(named string) xproto.Atom {
	reply, err := xproto.InternAtom(i.conn, false, uint16(len(named)), named).Reply()
	if err != nil {
		return 0
	}
	return reply.Atom
}

// quiet silences xgb, which reports on stderr what it did not find even though it connects fine
// without it.
func quiet() { xgb.Logger = log.New(io.Discard, "", 0) }
