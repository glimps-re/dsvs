package tui

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"dsvs/internal/core"
)

// Copying on X11 means taking ownership of the selections and then answering whoever asks for them:
// there is no clipboard anywhere but in the program that copied. The desktop's clipboard manager makes
// a copy right away, so there is nothing to keep for long. Measured on this machine: OSC 52 does not
// get through xfce4-terminal, and no external tool is installed — talking to X directly is the only
// way, and dsvs already does it in pure Go for its icon, with CGO disabled.

// held is the connection serving the current copy, if any. A copy replaces the previous one: closing
// the connection gives the selections back, and the next one takes them.
var held struct {
	sync.Mutex
	conn *xgb.Conn
}

// copyText puts what the mouse took on the clipboard. Without a display — a colleague over SSH, a pure
// Wayland terminal — all that is left is to ask the terminal to do it, which it does if it can.
func copyText(text string, journal *core.Journal) tea.Cmd {
	if text == "" {
		return nil
	}
	if os.Getenv("DISPLAY") == "" {
		return tea.SetClipboard(text)
	}
	return func() tea.Msg {
		if err := own(text); err != nil {
			journal.Addf("!! clipboard: %v", err)
		}
		return nil
	}
}

// own takes the CLIPBOARD and PRIMARY selections on this text and serves them in the background until
// another program takes them.
func own(text string) error {
	// xgb reports on stderr what it did not find — a missing .Xauthority, for instance — even though it
	// connects fine without it. A full-screen interface must not be soiled by that.
	xgb.Logger = log.New(io.Discard, "", 0)
	conn, err := xgb.NewConn()
	if err != nil {
		return fmt.Errorf("no X display: %w", err)
	}
	setup := xproto.Setup(conn)
	// The text travels in a single request: beyond that it would have to be split (INCR), which a log
	// has never required here.
	if limit := int(setup.MaximumRequestLength)*4 - 64; len(text) > limit {
		conn.Close()
		return fmt.Errorf("%d bytes is more than one X request carries (%d)", len(text), limit)
	}

	win, err := xproto.NewWindowId(conn)
	if err != nil {
		conn.Close()
		return err
	}
	root := setup.DefaultScreen(conn).Root
	if err := xproto.CreateWindowChecked(conn, 0, win, root, 0, 0, 1, 1, 0,
		xproto.WindowClassInputOnly, 0, 0, nil).Check(); err != nil {
		conn.Close()
		return err
	}

	named := atoms(conn, "CLIPBOARD", "TARGETS", "UTF8_STRING", "TEXT")
	selections := []xproto.Atom{named["CLIPBOARD"], xproto.AtomPrimary}
	for _, selection := range selections {
		xproto.SetSelectionOwner(conn, win, selection, xproto.TimeCurrentTime)
	}
	for _, selection := range selections {
		reply, err := xproto.GetSelectionOwner(conn, selection).Reply()
		if err != nil {
			conn.Close()
			return err
		}
		if reply.Owner != win {
			conn.Close()
			return fmt.Errorf("another program holds the selection")
		}
	}

	held.Lock()
	previous := held.conn
	held.conn = conn
	held.Unlock()
	if previous != nil {
		previous.Close()
	}

	go func() {
		serve(conn, named, text, len(selections))
		held.Lock()
		if held.conn == conn {
			held.conn = nil
		}
		held.Unlock()
		conn.Close()
	}()
	return nil
}

// serve answers whoever asks for the selection, and returns as soon as both have been taken back from
// us: the clipboard then belongs to someone else, who has done what they wanted with it.
func serve(conn *xgb.Conn, named map[string]xproto.Atom, text string, holding int) {
	for holding > 0 {
		event, err := conn.WaitForEvent()
		if event == nil {
			if err == nil {
				return
			}
			continue
		}
		switch e := event.(type) {
		case xproto.SelectionRequestEvent:
			answer(conn, e, named, text)
		case xproto.SelectionClearEvent:
			holding--
		}
	}
}

// answer serves a request: the list of what can be provided, or the text itself. A target that cannot
// be provided is refused with a None property, and the requestor is notified in every case.
func answer(conn *xgb.Conn, req xproto.SelectionRequestEvent, named map[string]xproto.Atom, text string) {
	// An obsolete requestor names no property: the convention is then to use the target.
	property := req.Property
	if property == xproto.AtomNone {
		property = req.Target
	}
	switch req.Target {
	case named["TARGETS"]:
		offered := []xproto.Atom{named["TARGETS"], named["UTF8_STRING"], named["TEXT"], xproto.AtomString}
		data := make([]byte, 4*len(offered))
		for i, target := range offered {
			xgb.Put32(data[4*i:], uint32(target))
		}
		xproto.ChangeProperty(conn, xproto.PropModeReplace, req.Requestor, property,
			xproto.AtomAtom, 32, uint32(len(offered)), data)
	case named["UTF8_STRING"], named["TEXT"], xproto.AtomString:
		xproto.ChangeProperty(conn, xproto.PropModeReplace, req.Requestor, property,
			req.Target, 8, uint32(len(text)), []byte(text))
	default:
		property = xproto.AtomNone
	}
	notify := xproto.SelectionNotifyEvent{Time: req.Time, Requestor: req.Requestor,
		Selection: req.Selection, Target: req.Target, Property: property}
	xproto.SendEvent(conn, false, req.Requestor, 0, string(notify.Bytes()))
}

func atoms(conn *xgb.Conn, names ...string) map[string]xproto.Atom {
	out := make(map[string]xproto.Atom, len(names))
	for _, name := range names {
		if reply, err := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply(); err == nil {
			out[name] = reply.Atom
		}
	}
	return out
}
