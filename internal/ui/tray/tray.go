package tray

import (
	"errors"
	"image"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"dsvs/internal/build"
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

	icon := &icon{conn: conn, screen: screen, size: side}
	if err := icon.show(); err != nil {
		return err
	}
	// The status bar may restart: it says so by taking the selection back, and that is when to ask for
	// a place again. Without it the icon vanishes until the next launch.
	xproto.ChangeWindowAttributes(conn, screen.Root, xproto.CwEventMask,
		[]uint32{xproto.EventMaskStructureNotify})
	icon.dock()
	return icon.live(open)
}

type icon struct {
	conn   *xgb.Conn
	screen *xproto.ScreenInfo
	win    xproto.Window
	gc     xproto.Gcontext
	size   int
	// shown, when non-nil, is the open panel: closing it closes the panel. It is also what says there is
	// one, and that a second click must remove it rather than place another.
	//
	// aiming is the click in progress, or nil: what to do if the button is released over the icon.
	mu     sync.Mutex
	shown  chan struct{}
	aiming *aim
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
				i.size = min(int(e.Width), int(e.Height))
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

// draw paints the icon: three bars on a dark tile, like a list of apps. Every pixel is painted —
// relying on the parent's background made the icon uniformly white (measured), the status bar giving
// us no background to inherit. What must stay readable is the silhouette, and a luminance gap is what
// survives whatever the status bar does to our pixels: the bar tint only says which binary this is.
func (i *icon) draw() {
	i.fill(0x101014, []xproto.Rectangle{{Width: uint16(i.size), Height: uint16(i.size)}})
	i.fill(tint(build.Name()), bars(i.size))
}

func (i *icon) fill(color uint32, boxes []xproto.Rectangle) {
	xproto.ChangeGC(i.conn, i.gc, xproto.GcForeground, []uint32{color})
	xproto.PolyFillRectangle(i.conn, xproto.Drawable(i.win), i.gc, boxes)
}

// bars returns the three bars at the size we were given: the status bar decides, not us.
func bars(size int) []xproto.Rectangle {
	thick := max(2, size/7)
	margin := max(2, size/6)
	long := size - 2*margin
	short := max(2, long*3/5)
	gap := (size - 3*thick) / 4
	out := make([]xproto.Rectangle, 0, 3)
	for row, width := range []int{long, short, long} {
		top := gap + row*(thick+gap)
		out = append(out, xproto.Rectangle{X: int16(margin), Y: int16(top),
			Width: uint16(width), Height: uint16(thick)})
	}
	return out
}

// tint is the bars' color: the working dsvs has them white, any other binary has them amber. The name
// is already what isolates the rest, it also decides the tint, and two icons in the same status bar
// tell apart without hovering.
func tint(name string) uint32 {
	if name == "dsvs" {
		return 0xf0f0f0
	}
	return 0xf0b040
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
