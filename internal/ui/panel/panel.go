package panel

import (
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomedium"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"

	"dsvs/internal/attach"
	"dsvs/internal/build"
	"dsvs/internal/hub"
	"dsvs/internal/projects"
	"dsvs/internal/update"
)

// escape is the keycode of the key that closes the panel — it does not depend on the keyboard layout,
// unlike a letter. It only arrives if the focus comes back to us, which this window manager does not
// give to a window outside its management: the cross is the safe way out, and it depends on nothing.
const escape = 9

// patience is how long an app waits for the state its order should cause: an order with no effect
// causes none, and the wait must end anyway.
const patience = 3 * time.Second

// glance is the pace at which we check whether someone just clicked elsewhere. Short enough to catch
// a quick click, long enough to cost nothing.
const glance = 50 * time.Millisecond

// Open opens the panel against this anchor, in screen coordinates: it is the rectangle of the icon
// that asked for it, and the panel sits on the side opposite the status bar. It calls gone as soon as
// the window is gone, and only returns once what it started is finished: these are two distinct
// facts, and whoever holds the icon needs the first to reopen on the next click.
//
// It also closes on a click elsewhere on this machine, which it learns by watching the pointer, not
// by grabbing it. So it touches nothing shared: no pointer grab, no keyboard grab, no focus request —
// except during an entry opened with a click. A grab confiscates the input of whoever is subjected to
// it — and under Qubes it would not even bring the click beside it, which a VM's X server never sees;
// a focus request earns us either the keystrokes of someone who thinks they are typing elsewhere, or
// an immediate FocusOut that closes the panel at random (both measured). An anchor with negative
// coordinates: at the pointer, where an icon's click would open it.
func Open(anchor image.Rectangle, until <-chan struct{}, gone func()) error {
	// xgb tells stderr what it did not find — a missing .Xauthority, for instance, while it connects
	// fine without one. A panel does not litter the terminal that opens it.
	xgb.Logger = log.New(io.Discard, "", 0)

	// An update started from here cannot be silently abandoned: the window closes when asked, but
	// Open only returns once the work is done. Otherwise a dsvs panel that leaves would leave a clone
	// halfway, and nobody to say so.
	var working sync.WaitGroup
	defer working.Wait()
	defer gone()

	with, err := load()
	if err != nil {
		return err
	}
	conn, err := xgb.NewConn()
	if err != nil {
		return fmt.Errorf("no X display: %w", err)
	}
	defer conn.Close()

	p := &panel{conn: conn, with: with, screen: xproto.Setup(conn).DefaultScreen(conn),
		limit: int(xproto.Setup(conn).MaximumRequestLength) * 4,
		msb:   xproto.Setup(conn).ImageByteOrder == xproto.ImageOrderMSBFirst,
		state: map[string]attach.State{}, links: map[string]*attach.Link{}, failed: map[string]string{},
		waking: map[string]bool{}, pending: map[string]map[string]time.Time{}, replaying: map[string]change{}, refused: map[string]string{},
		replayed: make(chan replay, 8), forgot: make(chan replay, 8), reports: make(chan report, 8), told: make(chan renewal, 8),
		awake: make(chan woken, 8), done: make(chan struct{}), working: &working}
	p.ordered, p.setting, p.keys, p.claim = p.through, attach.Set, mapping(conn), p.focus
	p.setting = func(config string, values map[string]string) error { return attach.Draft(config, values) }
	defer p.release()

	if err := p.reach(); err != nil {
		return err
	}
	for config, link := range p.links {
		go p.follow(config, link.States())
	}
	if err := p.show(anchor); err != nil {
		return err
	}
	// The question is asked on opening, not when the service starts: this is where its answer can be
	// shown, and a black-and-white icon could not announce it.
	p.news = looking
	go p.look()
	return p.live(until)
}

// Render writes the panel as a PNG, as the icon would open it now, without a window or an X server:
// the same drawing, pushed into a file rather than onto the screen. Nothing is hovered, and
// everything is unfolded.
func Render(w io.Writer) error {
	with, err := load()
	if err != nil {
		return err
	}
	p := &panel{state: map[string]attach.State{}, links: map[string]*attach.Link{}, done: make(chan struct{})}
	defer p.release()
	if err := p.reach(); err != nil {
		return err
	}
	for config, link := range p.links {
		if state, live := <-link.States(); live {
			p.state[config] = state
		}
	}
	canvas, _, _ := paint(p.snapshot(), renewal{stage: idle}, with, time.Now(), view{at: image.Pt(-1, -1), room: math.MaxInt})
	return png.Encode(w, canvas)
}

// report is a state that arrives from a session, or its departure.
type report struct {
	config string
	state  attach.State
	gone   bool
}

// replay is the answer to a gesture that waits for a session — a parameter change, a forget: nil if
// it took.
type replay struct {
	config string
	err    error
}

// woken is what waking a project gave: the link to its session, or the reason there is none.
type woken struct {
	config string
	link   *attach.Link
	err    error
}

type panel struct {
	conn   *xgb.Conn
	screen *xproto.ScreenInfo
	with   faces
	limit  int
	msb    bool

	win xproto.Window
	gc  xproto.Gcontext

	// order keeps the sessions in a stable order: a panel whose rows jump from one refresh to the
	// next cannot be clicked.
	order []string
	state map[string]attach.State
	links map[string]*attach.Link
	// failed carries, per project, what a gesture gave: it stays shown where it was made, until the
	// next gesture.
	failed map[string]string
	// waking are the projects whose session is coming up: they no longer offer to start it, which is
	// also what keeps a second click from starting a second session.
	waking map[string]bool
	// pending says, per project, until when each app whose order has left waits for its session's
	// next state, which tells what the order gave: an order that changes nothing causes none, and the
	// wait then stops at its deadline.
	pending map[string]map[string]time.Time
	// unfolded is the project chosen to unfold when the panel overflows the screen: it only lasts one
	// opening.
	unfolded string
	// replaying is, per project, the parameter change its session is replaying, and refused the
	// reason for the last one it refused: it is cleared on the next change, like the other reasons.
	replaying map[string]change
	refused   map[string]string
	// dropped is the parameter whose list of values is unfolded: only one at a time, and none once a
	// value is chosen.
	dropped slot
	// typing is the entry in progress of a text or a number, and keys the table that translates keys
	// into what they write. Any other gesture abandons it, like Escape: a single place holds the
	// keyboard, and it is the one in view. giveBack returns the focus the entry took, and exists only
	// with it; keyless is the parameter whose entry did not get the focus, and says so until the next
	// gesture.
	typing   draft
	keys     keyboard
	giveBack func()
	keyless  slot
	// claim takes the keyboard focus for an entry, and returns how to give it back; false if the
	// server did not grant it. A field, so that a test can do both without an X server.
	claim    func() (giveBack func(), taken bool)
	reports  chan report
	awake    chan woken
	replayed chan replay
	// forgot carries the answer to a forget: nil if the project is forgotten, the reason for the
	// refusal otherwise.
	forgot chan replay

	// setting changes a parameter of a project's session and returns once it has replayed: like
	// ordered, a field so that a test hears what the panel asks.
	setting func(config string, values map[string]string) error

	// ordered carries an order to a project's session. It is a field so that a test hears what the
	// panel orders without real sessions to lend it.
	ordered func(config string, do deed, name string) error

	// done closes when the panel leaves: what worked for it stops waiting for it. working counts what
	// it must not abandon along the way all the same.
	done    chan struct{}
	working *sync.WaitGroup

	// news is what is known about the update, and told what the goroutines looking for it or doing it
	// report: a single loop decides the display.
	news renewal
	told chan renewal

	zones []zone
	// rows are the app rows as the drawing laid them out: it is the one that knows where they are,
	// and hover is compared against them rather than redoing its geometry.
	rows   []image.Rectangle
	size   image.Point
	origin image.Point
	// anchor is the rectangle the panel sat against — the icon that opened it. It moves back against
	// it when its content changes size: the screen never said where the status bar was, the anchor
	// does.
	anchor image.Rectangle
	// room is the height the screen leaves to the panel, which folds when it exceeds it.
	room int
	// armed says the button was seen released since opening: the click that brought up the panel must
	// not close it right away.
	armed bool
	// at is the pointer in the panel. Outside it, it points outside the image and nothing lights up.
	at image.Point
	// press is the press in progress, nil when the button is not ours. It carries the zone where it
	// began, the empty zone if it was between two buttons: a click only acts if it is released where
	// it began, and it belongs to us until then, wherever it drags.
	press *zone
}

// reach opens a link with each session the hub knows, then lines up behind them the projects already
// seen that are not running. A session that does not answer is probably leaving: it is ignored rather
// than refusing to open the panel.
func (p *panel) reach() error {
	living, err := hub.Sessions()
	if err != nil {
		return err
	}
	for _, session := range living {
		link, err := attach.Watch(session.Config)
		if err != nil {
			continue
		}
		p.links[session.Config] = link
		p.order = append(p.order, session.Config)
	}
	slices.Sort(p.order)
	for _, config := range projects.List() {
		if !slices.Contains(p.order, config) {
			p.order = append(p.order, config)
		}
	}
	return nil
}

// wake starts a project's session cold and attaches to it, outside the loop: a session coming up
// takes up to five seconds to open its socket, and a panel that stops responding meanwhile is not a
// panel. What it gave comes back through the channel, like the rest.
func (p *panel) wake(config string) {
	link, err := attach.Ensure(config, func() error { return spawn(config) })
	select {
	case p.awake <- woken{config: config, link: link, err: err}:
	case <-p.done:
		if link != nil {
			link.Close()
		}
	}
}

// spawn starts a session without an interface for this configuration. It is the running binary that
// is restarted, never a name from PATH: a branch binary does not start the sessions of the working
// dsvs.
func spawn(config string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	session := exec.Command(exe, "--session", "--config", config)
	session.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return session.Start()
}

// follow carries a session's states into the panel's channel: a single event loop, whatever the
// number of sessions. Once the panel is gone, it stops: the service reopens a panel on every icon
// click, and goroutines left on a send nobody reads would never go away.
func (p *panel) follow(config string, states <-chan attach.State) {
	for state := range states {
		select {
		case p.reports <- report{config: config, state: state}:
		case <-p.done:
			return
		}
	}
	select {
	case p.reports <- report{config: config, gone: true}:
	case <-p.done:
	}
}

func (p *panel) release() {
	p.end()
	close(p.done)
	for _, link := range p.links {
		link.Close()
	}
}

// show creates the window and displays it. Undecorated and outside the window manager's management: a
// panel opens where it was asked for, like a menu.
func (p *panel) show(anchor image.Rectangle) error {
	if anchor.Min.X < 0 || anchor.Min.Y < 0 {
		x, y := p.pointer()
		anchor = image.Rectangle{Min: image.Pt(x, y), Max: image.Pt(x, y)}
	}
	p.anchor = anchor
	p.at = image.Pt(-1, -1)
	screens := p.screens()
	p.room = available(p.anchor, screens)
	canvas, zones, rows := paint(p.snapshot(), p.news, p.with, time.Now(), p.view())
	p.zones, p.rows, p.size = zones, rows, canvas.Bounds().Size()
	p.origin = beside(p.anchor, p.size, screens)
	x, y := p.origin.X, p.origin.Y

	win, err := xproto.NewWindowId(p.conn)
	if err != nil {
		return err
	}
	p.win = win
	if err := xproto.CreateWindowChecked(p.conn, p.screen.RootDepth, win, p.screen.Root,
		int16(x), int16(y), uint16(p.size.X), uint16(p.size.Y), 0,
		xproto.WindowClassInputOutput, p.screen.RootVisual,
		xproto.CwBackPixel|xproto.CwOverrideRedirect|xproto.CwEventMask,
		[]uint32{0x1c1d22, 1,
			xproto.EventMaskExposure | xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease |
				xproto.EventMaskKeyPress | xproto.EventMaskPointerMotion | xproto.EventMaskLeaveWindow |
				xproto.EventMaskFocusChange,
		}).Check(); err != nil {
		return err
	}
	name(p.conn, win, build.Label())

	gc, err := xproto.NewGcontextId(p.conn)
	if err != nil {
		return err
	}
	p.gc = gc
	if err := xproto.CreateGCChecked(p.conn, gc, xproto.Drawable(win), 0, nil).Check(); err != nil {
		return err
	}
	if err := xproto.MapWindowChecked(p.conn, win).Check(); err != nil {
		return err
	}
	p.push(canvas)
	return nil
}

// live holds the panel until it closes: changing states redraw it, clicks act.
func (p *panel) live(until <-chan struct{}) error {
	events := make(chan xgb.Event, 8)
	go func() {
		defer close(events)
		for {
			event, err := p.conn.WaitForEvent()
			if event == nil && err == nil {
				return
			}
			if event == nil {
				continue
			}
			select {
			case events <- event:
			case <-p.done:
				return
			}
		}
	}()

	away := time.NewTicker(glance)
	defer away.Stop()

	for {
		select {
		case <-away.C:
			if p.clickedAway() {
				return nil
			}
			if p.expire(time.Now()) {
				p.render()
			}

		case <-until:
			return nil // we are called back: a second click on the icon, for instance

		case news := <-p.told:
			p.news = news
			p.render()

		case up := <-p.awake:
			delete(p.waking, up.config)
			if up.err != nil {
				p.failed[up.config] = up.err.Error()
			} else {
				p.links[up.config] = up.link
				go p.follow(up.config, up.link.States())
			}
			p.render()

		case up := <-p.replayed:
			p.answered(up)
			p.render()

		case up := <-p.forgot:
			p.forgotten(up)
			p.render()

		case up := <-p.reports:
			delete(p.pending, up.config)
			if up.gone {
				delete(p.state, up.config)
				delete(p.links, up.config)
				// A remembered project stays in the list when its session leaves: that is the whole
				// point of remembering it. A forgotten one leaves with it.
				if !slices.Contains(projects.List(), up.config) {
					p.order = slices.DeleteFunc(p.order, func(other string) bool { return other == up.config })
				}
			} else {
				p.state[up.config] = up.state
			}
			p.render()

		case event, live := <-events:
			if !live {
				return nil
			}
			switch e := event.(type) {
			case xproto.ExposeEvent:
				p.render()
			case xproto.KeyPressEvent:
				if !p.pressed(e) {
					return nil
				}
				p.render()
			case xproto.MappingNotifyEvent:
				p.keys = mapping(p.conn)
			case xproto.FocusOutEvent:
				if e.Mode == xproto.NotifyModeNormal {
					p.lost()
					p.render()
				}
			case xproto.MotionNotifyEvent:
				p.follows(int(e.EventX), int(e.EventY))
			case xproto.LeaveNotifyEvent:
				p.follows(-1, -1)
			case xproto.ButtonPressEvent:
				p.grab(int(e.EventX), int(e.EventY))
				p.render()
			case xproto.ButtonReleaseEvent:
				// A click beside a button does nothing: the cross is there to leave.
				if !p.clicked(int(e.EventX), int(e.EventY)) {
					return nil
				}
				// A gesture shows right away: what it changed is already in the state.
				p.render()
			}
		}
	}
}

// expire forgets the waits whose deadline has passed, and says whether one of them was still shown.
func (p *panel) expire(now time.Time) bool {
	expired := false
	for config, apps := range p.pending {
		for name, until := range apps {
			if !until.After(now) {
				delete(apps, name)
				expired = true
			}
		}
		if len(apps) == 0 {
			delete(p.pending, config)
		}
	}
	return expired
}

// clicked carries a released button to the panel, and says whether it stays open. The gesture only
// acts if released on the zone where it began: pressed elsewhere, or dragged off the intended button,
// it is abandoned — that is what a click is expected to do, and what leaves room to change one's
// mind.
func (p *panel) clicked(x, y int) bool {
	aimed := p.press
	p.press = nil
	if z, hit := p.hit(x, y); hit && aimed != nil && z == *aimed {
		return p.act(z)
	}
	return true
}

// grab holds on to a press: the gesture is ours until its release, wherever it goes.
func (p *panel) grab(x, y int) {
	z, _ := p.hit(x, y)
	p.press = &z
}

// clickedAway says someone just clicked elsewhere. The pointer is queried instead of grabbed: a grab
// would route through us the clicks meant for this machine's other windows, and a panel that froze
// would keep them. Here nothing is kept — the click reaches its target, and the panel goes away.
//
// A click in another qube does not come down here: dom0 holds the pointer, and the other qubes'
// panels live with the same limit.
func (p *panel) clickedAway() bool {
	where, err := xproto.QueryPointer(p.conn, p.screen.Root).Reply()
	if err != nil {
		return false
	}
	const buttons = xproto.ButtonMask1 | xproto.ButtonMask2 | xproto.ButtonMask3
	return p.saw(image.Pt(int(where.RootX), int(where.RootY)), where.Mask&buttons != 0)
}

// saw reads a pointer state and says whether the panel must go away. A pressed button only counts
// after it was seen released: otherwise the click that brought up the panel would remove it straight
// away. And a press that is ours stays ours: dragging out of the frame is the right to change one's
// mind, not a click elsewhere.
func (p *panel) saw(pointer image.Point, down bool) bool {
	if !down {
		p.armed = true
		return false
	}
	return p.armed && p.press == nil && !pointer.In(p.frame())
}

// frame is the panel in screen coordinates.
func (p *panel) frame() image.Rectangle {
	return image.Rectangle{Min: p.origin, Max: p.origin.Add(p.size)}
}

// follows tracks the pointer: what is under it lights up, and nothing is redrawn if it has not
// changed zone — a panel that repaints on every pixel crossed would flicker.
func (p *panel) follows(x, y int) {
	was, _ := p.hit(p.at.X, p.at.Y)
	wasRow := rowAt(p.rows, p.at)
	p.at = image.Pt(x, y)
	now, _ := p.hit(x, y)
	if was != now || wasRow != rowAt(p.rows, p.at) {
		p.render()
	}
}

// rowAt returns the row containing this point, -1 when it touches none.
func rowAt(rows []image.Rectangle, at image.Point) int {
	for i, row := range rows {
		if at.In(row) {
			return i
		}
	}
	return -1
}

// hit says which button was clicked.
func (p *panel) hit(x, y int) (zone, bool) {
	for _, z := range p.zones {
		if image.Pt(x, y).In(z.at) {
			return z, true
		}
	}
	return zone{}, false
}

// pressed carries a key to the panel, and says whether it stays open. With no entry in progress, only
// Escape means something: it closes it. During an entry, Enter sends the value, Escape abandons it,
// and the rest is written.
func (p *panel) pressed(e xproto.KeyPressEvent) bool {
	if p.typing.name == "" {
		return e.Detail != escape
	}
	switch sym := p.keys.symbol(e.Detail, e.State); sym {
	case xkReturn, xkKPEnter:
		p.change(p.typing.config, p.typing.name, string(p.typing.text))
	case xkEscape:
		p.end()
	default:
		p.typing = p.typing.edited(sym)
	}
	return true
}

// end closes the entry in progress, if any, and gives back the focus it took.
func (p *panel) end() {
	if p.giveBack != nil {
		p.giveBack()
	}
	p.typing, p.giveBack = draft{}, nil
}

// lost closes the entry whose focus went elsewhere, without taking it back from whoever has it: keys
// no longer arrive, and a box that looked like it was writing would lie.
func (p *panel) lost() {
	p.giveBack = nil
	p.end()
}

// focus takes the keyboard focus for an entry, and says whether the server granted it: asking is not
// enough, it must be read back. Giving it back restores the previous one, where the typing was before
// the click.
func (p *panel) focus() (func(), bool) {
	before, err := xproto.GetInputFocus(p.conn).Reply()
	if err != nil {
		return nil, false
	}
	if err := xproto.SetInputFocusChecked(p.conn, xproto.InputFocusParent, p.win, xproto.TimeCurrentTime).Check(); err != nil {
		return nil, false
	}
	if now, err := xproto.GetInputFocus(p.conn).Reply(); err != nil || now.Focus != p.win {
		return nil, false
	}
	return func() { xproto.SetInputFocus(p.conn, before.RevertTo, before.Focus, xproto.TimeCurrentTime) }, true
}

// act carries the click to whom it concerns, and says whether the panel stays open.
func (p *panel) act(z zone) bool {
	p.end()
	p.keyless = slot{}
	switch z.do {
	case dismiss:
		return false
	case open:
		p.waking[z.config] = true
		delete(p.failed, z.config)
		go p.wake(z.config)
		return true
	case forget:
		go p.forgetting(z.config)
		return true
	case start, stop, restart:
		if p.command(z.config, z.do, z.name) {
			if p.pending[z.config] == nil {
				p.pending[z.config] = map[string]time.Time{}
			}
			p.pending[z.config][z.name] = time.Now().Add(patience)
		}
		return true
	case unfold:
		p.unfolded = z.config
		return true
	case drop:
		if clicked := (slot{z.config, z.name}); p.dropped == clicked {
			p.dropped = slot{}
		} else {
			p.dropped = clicked
		}
		return true
	case choose:
		p.change(z.config, z.name, z.value)
		return true
	case edit:
		p.dropped = slot{}
		giveBack, taken := p.claim()
		if !taken {
			p.keyless = slot{z.config, z.name}
			return true
		}
		p.giveBack = giveBack
		p.typing = draft{slot: slot{z.config, z.name}, text: []rune(z.value), at: len([]rune(z.value))}
		return true
	case stopEverything:
		for config := range p.links {
			p.command(config, closeAll, "")
		}
		return true
	case quitDsvs:
		p.leave()
		return false
	case browse:
		// The panel stays open: two facades are often opened in a row, and xdg-open has nothing to
		// give back — waiting for it would block the loop on a browser starting up.
		p.reported(z.config, exec.Command("xdg-open", z.name).Start())
		return true
	case look:
		// This is where we know it leaves, so this is where the button goes away: the painter only
		// shows the state, it does not have to keep the two-click rule.
		p.news = looking
		go p.look()
		return true
	case renew:
		p.work("starting the update", p.renew)
		return true
	case installBeta:
		p.work("installing the beta", p.addBeta)
		return true
	case removeBeta:
		p.work("uninstalling the beta", p.dropBeta)
		return true
	case openTUI:
		// The panel only goes away if the interface really opens: on a machine with no known
		// terminal, it stays in place rather than saying nothing at all.
		return !p.reported(z.config, p.callTUI(z.config))
	}
	p.command(z.config, z.do, z.name)
	return true
}

// change gives a value to a parameter, outside the loop: the session replays its configuration, which
// can take the time of an evaluation, and a panel that stops responding meanwhile is not one. What it
// answered comes back through the channel, like the rest.
func (p *panel) change(config, name, value string) {
	p.replaying[config] = change{name, value}
	delete(p.refused, config)
	p.dropped = slot{}
	p.end()
	go func() {
		err := p.setting(config, map[string]string{name: value})
		select {
		case p.replayed <- replay{config, err}:
		case <-p.done:
		}
	}()
}

// forgetting forgets a project outside the loop: its session, when it has one, replays its
// configuration before answering.
func (p *panel) forgetting(config string) {
	_, _, err := attach.Forget(config)
	select {
	case p.forgot <- replay{config, err}:
	case <-p.done:
	}
}

// forgotten files what a forget gave: the reason for a refusal on the project's row, or its row
// removed — except when its session, opened with no app running, stays open: it keeps the row while
// it lives.
func (p *panel) forgotten(up replay) {
	if _, live := p.state[up.config]; p.reported(up.config, up.err) && !live {
		p.order = slices.DeleteFunc(p.order, func(other string) bool { return other == up.config })
	}
}

// answered hands control back to the parameters of a session that answered, and keeps its reason if
// it refused: the shown value then goes back to the one the session reports, since its state did not
// change.
func (p *panel) answered(up replay) {
	delete(p.replaying, up.config)
	if up.err != nil {
		p.refused[up.config] = up.err.Error()
	}
}

// command carries an order to a project's session, and says whether it left.
func (p *panel) command(config string, do deed, name string) bool {
	return p.reported(config, p.ordered(config, do, name))
}

// reported keeps on a project's row what a gesture gave, clears it when it worked, and says whether
// it worked: a dead socket or a terminal not found used to leave a click without the slightest
// answer, where the project's row is made for that.
func (p *panel) reported(config string, err error) bool {
	if err != nil {
		p.failed[config] = err.Error()
		return false
	}
	delete(p.failed, config)
	return true
}

// through is an order's path: the link opened with the session.
func (p *panel) through(config string, do deed, name string) error {
	link := p.links[config]
	if link == nil {
		return fmt.Errorf("no session serves %s", config)
	}
	return link.Order(string(do), name)
}

// look searches for a new version. The clone answers for free, the remote repository takes the time
// it takes: hence the background, and the step shown meanwhile.
func (p *panel) look() {
	newer, err := update.Newer()
	var news renewal
	switch {
	case err != nil:
		news = failure(err)
	case newer == "":
		news = renewal{stage: idle, said: "up to date"}
	default:
		news = renewal{stage: found, to: newer}
	}
	news.beta, news.betaOut = besideBeta()
	p.tell(news)
}

// besideBeta is the gesture this binary offers on the beta: dsvs installs one once it is out, the
// beta uninstalls itself, and any other binary has nothing to do with it — nor has a binary built
// from a clone, which follows its clone and never the releases. Why no beta is offered is not worth a
// line: the update says it already when the releases are out of reach.
func besideBeta() (deed, string) {
	switch {
	case build.Origin != "":
		return nothing, ""
	case build.Name() == build.Beta:
		return removeBeta, ""
	}
	if out, _ := update.BetaToInstall(); out != "" {
		return installBeta, out
	}
	return nothing, ""
}

// looking is what a search that just started shows: its step, and nothing to click.
var looking = doing("looking for a newer version")

// work runs a long gesture in the background, showing its steps as it goes. The panel closes when
// asked, but Open only returns once the gesture is over.
func (p *panel) work(first string, gesture func()) {
	p.news = doing(first)
	p.working.Add(1)
	go func() {
		defer p.working.Done()
		gesture()
	}()
}

// renew takes the version found, showing the current step. The panel goes away with the service it
// has restarted: the last steps have no one to tell, and that is fine — the icon coming back is the
// sign that was needed.
func (p *panel) renew() {
	if err := update.Apply(p.step); err != nil {
		p.over("update", "", err)
	}
}

// addBeta installs the beta beside this binary: its icon comes up next to this one.
func (p *panel) addBeta() {
	said, err := update.InstallBeta(p.step)
	p.over("installing the beta", said, err)
}

// dropBeta uninstalls the beta this panel belongs to. Its service leaves last, and the panel with it:
// what follows is only read when there was no service to stop.
func (p *panel) dropBeta() {
	p.over("uninstalling the beta", build.Beta+" is uninstalled", update.RemoveBeta(p.step))
}

func (p *panel) step(said string) { p.tell(doing(said)) }

// over says how a long gesture ended. A failure also goes to stderr: the panel may have closed while
// it worked, and that is then the service's journal, or the terminal of dsvs panel — a failure read
// nowhere is the one thing not allowed.
func (p *panel) over(gesture, said string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s failed: %v\n", gesture, err)
		p.tell(failure(err))
		return
	}
	p.tell(renewal{stage: idle, said: said})
}

// tell carries news to the panel's loop. It waits until the loop has taken it, or until the panel has
// gone: a step dropped for lack of room would be the missing one, and a closed panel holds nobody
// back.
func (p *panel) tell(news renewal) {
	select {
	case p.told <- news:
	case <-p.done:
	}
}

// leave goes away for good: the sessions stop, then the service — its icon with it. The order
// matters, the panel possibly living in the process that holds the icon: it is the last word.
func (p *panel) leave() {
	for config := range p.links {
		p.command(config, closeAll, "")
	}
	conn, err := net.Dial("unix", hub.TrayAddr())
	if err != nil {
		return // no service to stop: the panel closes, and that is all
	}
	defer conn.Close()
	fmt.Fprintln(conn, "quit")
}

// callTUI brings this session's TUI back on screen, or opens one in a new terminal.
func (p *panel) callTUI(config string) error {
	for _, seen := range p.state[config].Interfaces {
		if seen.Kind != "tui" || seen.Window == "" || seen.Window == "-" {
			continue
		}
		if id, err := strconv.ParseUint(seen.Window, 10, 32); err == nil {
			p.demand(xproto.Window(id))
			return nil
		}
	}
	return openTerminal(config)
}

// demand asks for attention for this window. That is all this window manager claims to know how to do
// — _NET_SUPPORTED lists neither _NET_ACTIVE_WINDOW nor _NET_CLIENT_LIST — but both are asked: the
// second request costs nothing where it is not understood.
func (p *panel) demand(win xproto.Window) {
	send := func(kind xproto.Atom, data []uint32) {
		message := xproto.ClientMessageEvent{Format: 32, Window: win, Type: kind,
			Data: xproto.ClientMessageDataUnionData32New(data)}
		xproto.SendEvent(p.conn, false, p.screen.Root,
			xproto.EventMaskSubstructureNotify|xproto.EventMaskSubstructureRedirect, string(message.Bytes()))
	}
	send(atom(p.conn, "_NET_ACTIVE_WINDOW"), []uint32{2, uint32(xproto.TimeCurrentTime), 0, 0, 0})
	send(atom(p.conn, "_NET_WM_STATE"),
		[]uint32{1 /* ajouter */, uint32(atom(p.conn, "_NET_WM_STATE_DEMANDS_ATTENTION")), 0, 2, 0})
}

// terminals are the terminals we know how to start, and the option by which each accepts a command.
// There is no portable spelling for it: it is a table, and it grows as needed.
var terminals = []struct{ name, flag string }{
	{"xfce4-terminal", "-x"},
	{"gnome-terminal", "--"},
	{"konsole", "-e"},
	{"xterm", "-e"},
	{"x-terminal-emulator", "-e"},
}

// openTerminal opens a TUI in a new terminal: --tui, since the bare binary writes the session's state
// and returns. It is the running binary that is restarted, never a name looked up in PATH: a branch
// binary must not open the working dsvs.
func openTerminal(config string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	for _, terminal := range terminals {
		path, err := exec.LookPath(terminal.name)
		if err != nil {
			continue
		}
		return exec.Command(path, terminal.flag, exe, "--tui", "--config", config).Start()
	}
	names := make([]string, len(terminals))
	for i, terminal := range terminals {
		names[i] = terminal.name
	}
	return fmt.Errorf("no terminal found: tried %s", strings.Join(names, ", "))
}

// render redraws the panel on the current state, and moves the window when the content changed size.
// An app that appears, a failure line or an update make the panel grow: opened near an edge, it would
// go past it, whereas it had been placed on opening.
func (p *panel) render() {
	canvas, zones, rows := paint(p.snapshot(), p.news, p.with, time.Now(), p.view())
	p.zones, p.rows = zones, rows
	if size := canvas.Bounds().Size(); size != p.size {
		p.size = size
		p.origin = beside(p.anchor, p.size, p.screens())
		xproto.ConfigureWindow(p.conn, p.win,
			xproto.ConfigWindowX|xproto.ConfigWindowY|xproto.ConfigWindowWidth|xproto.ConfigWindowHeight,
			[]uint32{uint32(p.origin.X), uint32(p.origin.Y), uint32(size.X), uint32(size.Y)})
	}
	p.push(canvas)
}

// view is what the hand is doing with the panel, so that the drawing shows it.
func (p *panel) view() view {
	seen := view{at: p.at, room: p.room, unfolded: p.unfolded, dropped: p.dropped, typing: p.typing, keyless: p.keyless}
	if p.press != nil {
		seen.pressed = *p.press
	}
	return seen
}

// snapshot returns the projects in the panel's order: the running sessions, then what was already
// seen and is asleep.
func (p *panel) snapshot() []project {
	out := make([]project, 0, len(p.order))
	for _, config := range p.order {
		one := project{config: config, failed: p.failed[config], waking: p.waking[config], pending: p.pending[config],
			refused: p.refused[config]}
		if replaying, going := p.replaying[config]; going {
			one.replaying = &replaying
		}
		if state, live := p.state[config]; live {
			one.live = &state
		}
		out = append(out, one)
	}
	return out
}

// push sends the image to the server in strips: a request cannot exceed the size the server
// announces, and a whole panel exceeds it.
func (p *panel) push(canvas *image.RGBA) {
	bounds := canvas.Bounds()
	stride, rows := bounds.Dx()*4, max(1, (p.limit-64)/(bounds.Dx()*4))
	for top := 0; top < bounds.Dy(); top += rows {
		high := min(rows, bounds.Dy()-top)
		band := make([]byte, stride*high)
		for y := range high {
			line := canvas.Pix[(top+y)*canvas.Stride:]
			for x := 0; x < stride; x += 4 {
				r, g, b := line[x], line[x+1], line[x+2]
				if p.msb {
					band[y*stride+x+1], band[y*stride+x+2], band[y*stride+x+3] = r, g, b
					continue
				}
				band[y*stride+x], band[y*stride+x+1], band[y*stride+x+2] = b, g, r
			}
		}
		xproto.PutImage(p.conn, xproto.ImageFormatZPixmap, xproto.Drawable(p.win), p.gc,
			uint16(bounds.Dx()), uint16(high), 0, int16(top), 0, p.screen.RootDepth, band)
	}
}

// edge is what a panel pushed back leaves between itself and the screen's edge, gap what it leaves
// between itself and its anchor.
const (
	edge = 4
	gap  = 4
)

// beside places a panel of this size against this anchor, in the screen that receives it or the
// nearest one. The screen edge the anchor is closest to is the status bar's, and the panel sits on
// the opposite side: that is all that can be known of a bar that lives elsewhere and publishes
// neither work area nor struts. An empty anchor is a point — the pointer, when there is no icon — and
// the same computation surrounds it the same way.
//
// Fitting into the screens' bounding box is not enough: two offset monitors leave dead strips in it,
// and a panel placed in one is visible from nowhere — measured, its cross became unreachable. Taller
// than the screen, it overflows at the bottom: its cross is at the top, and that is what counts.
func beside(anchor image.Rectangle, size image.Point, screens []image.Rectangle) image.Point {
	middle := anchor.Min.Add(anchor.Max).Div(2)
	home := nearest(middle, screens).Inset(edge)
	if home.Empty() {
		return anchor.Min
	}
	top, bottom := middle.Y-home.Min.Y, home.Max.Y-middle.Y
	left, right := middle.X-home.Min.X, home.Max.X-middle.X
	var at image.Point
	switch away := min(top, bottom, left, right); {
	case away == top:
		at = image.Pt(anchor.Min.X, anchor.Max.Y+gap)
	case away == bottom:
		at = image.Pt(anchor.Min.X, anchor.Min.Y-gap-size.Y)
	case away == left:
		at = image.Pt(anchor.Max.X+gap, anchor.Min.Y)
	default:
		at = image.Pt(anchor.Min.X-gap-size.X, anchor.Min.Y)
	}
	return image.Pt(clamp(at.X, home.Min.X, max(home.Min.X, home.Max.X-size.X)),
		clamp(at.Y, home.Min.Y, max(home.Min.Y, home.Max.Y-size.Y)))
}

// available is the height the panel can take: that of the screen receiving the anchor, minus the
// icon, the gap and both edges.
func available(anchor image.Rectangle, screens []image.Rectangle) int {
	home := nearest(anchor.Min.Add(anchor.Max).Div(2), screens).Inset(edge)
	return home.Dy() - anchor.Dy() - gap
}

// nearest returns the screen containing this point, or the one whose center is closest to it: a click
// always lands somewhere, even in a dead strip.
func nearest(at image.Point, screens []image.Rectangle) image.Rectangle {
	var home image.Rectangle
	best := 0
	for _, screen := range screens {
		if at.In(screen) {
			return screen
		}
		middle := screen.Min.Add(screen.Max).Div(2)
		away := (at.X-middle.X)*(at.X-middle.X) + (at.Y-middle.Y)*(at.Y-middle.Y)
		if home.Empty() || away < best {
			home, best = screen, away
		}
	}
	return home
}

// screens returns the real monitors, through RandR. Without it, there is only the X screen's bounding
// box: that is what we had, and that is what put a panel in the void.
func (p *panel) screens() []image.Rectangle {
	whole := []image.Rectangle{{Max: image.Pt(int(p.screen.WidthInPixels), int(p.screen.HeightInPixels))}}
	if err := randr.Init(p.conn); err != nil {
		return whole
	}
	resources, err := randr.GetScreenResourcesCurrent(p.conn, p.screen.Root).Reply()
	if err != nil {
		return whole
	}
	var out []image.Rectangle
	for _, crtc := range resources.Crtcs {
		info, err := randr.GetCrtcInfo(p.conn, crtc, resources.ConfigTimestamp).Reply()
		// A switched-off output has zero width: it is not a screen to place anything on.
		if err != nil || info.Width == 0 || info.Height == 0 {
			continue
		}
		out = append(out, image.Rect(int(info.X), int(info.Y),
			int(info.X)+int(info.Width), int(info.Y)+int(info.Height)))
	}
	if len(out) == 0 {
		return whole
	}
	return out
}

// pointer returns the pointer's position: that is where a panel asked for without coordinates opens.
func (p *panel) pointer() (int, int) {
	where, err := xproto.QueryPointer(p.conn, p.screen.Root).Reply()
	if err != nil {
		return 0, 0
	}
	return int(where.RootX), int(where.RootY)
}

// load reads the panel's fonts. They are embedded in the binary: nothing to install, and the same
// rendering everywhere.
func load() (faces, error) {
	var with faces
	for _, one := range []struct {
		into   *font.Face
		raw    []byte
		points float64
	}{
		{&with.text, goregular.TTF, 12},
		{&with.bold, gobold.TTF, 13},
		{&with.small, gomedium.TTF, 11},
		{&with.mono, gomono.TTF, 11},
	} {
		parsed, err := sfnt.Parse(one.raw)
		if err != nil {
			return faces{}, err
		}
		if *one.into, err = opentype.NewFace(parsed, &opentype.FaceOptions{Size: one.points, DPI: 96, Hinting: font.HintingFull}); err != nil {
			return faces{}, err
		}
	}
	return with, nil
}

func name(conn *xgb.Conn, win xproto.Window, text string) {
	for _, property := range []string{"WM_NAME", "_NET_WM_NAME"} {
		xproto.ChangeProperty(conn, xproto.PropModeReplace, win, atom(conn, property),
			atom(conn, "UTF8_STRING"), 8, uint32(len(text)), []byte(text))
	}
	class := append([]byte("dsvs\x00"), []byte("Dsvs\x00")...)
	xproto.ChangeProperty(conn, xproto.PropModeReplace, win, atom(conn, "WM_CLASS"),
		atom(conn, "STRING"), 8, uint32(len(class)), class)
}

func atom(conn *xgb.Conn, named string) xproto.Atom {
	reply, err := xproto.InternAtom(conn, false, uint16(len(named)), named).Reply()
	if err != nil {
		return 0
	}
	return reply.Atom
}

func clamp(value, low, high int) int {
	return max(low, min(value, high))
}
