package tui

import (
	"os"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// Copying takes the X11 selections and serves them: there is no clipboard anywhere but in the program
// that copied. This test checks it from a second connection, as another program would — so it writes to
// the machine's clipboard, and only makes sense where there is a display.
func TestCopyOwnsTheX11Selections(t *testing.T) {
	if os.Getenv("DISPLAY") == "" {
		t.Skip("no X display")
	}
	const text = "dsvs clipboard test"
	if err := own(text); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		held.Lock()
		defer held.Unlock()
		if held.conn != nil {
			held.conn.Close()
			held.conn = nil
		}
	})
	for _, name := range []string{"CLIPBOARD", "PRIMARY"} {
		if got := pasted(t, name); got != text {
			t.Errorf("%s holds %q, want %q", name, got, text)
		}
	}
}

// pasted reads a selection back as a paster would: its own connection, its own window, and the answer
// arriving in the property it named.
func pasted(t *testing.T, name string) string {
	t.Helper()
	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	win, err := xproto.NewWindowId(conn)
	if err != nil {
		t.Fatal(err)
	}
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	if err := xproto.CreateWindowChecked(conn, 0, win, root, 0, 0, 1, 1, 0,
		xproto.WindowClassInputOnly, 0, 0, nil).Check(); err != nil {
		t.Fatal(err)
	}
	named := atoms(conn, name, "UTF8_STRING", "DSVS_PASTE")
	xproto.ConvertSelection(conn, win, named[name], named["UTF8_STRING"], named["DSVS_PASTE"],
		xproto.TimeCurrentTime)

	answered := make(chan xproto.SelectionNotifyEvent, 1)
	go func() {
		for {
			event, err := conn.WaitForEvent()
			if event == nil && err == nil {
				return
			}
			if notify, ok := event.(xproto.SelectionNotifyEvent); ok {
				answered <- notify
				return
			}
		}
	}()
	select {
	case notify := <-answered:
		if notify.Property == xproto.AtomNone {
			t.Fatalf("%s: the request was refused", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: nobody answered", name)
	}

	reply, err := xproto.GetProperty(conn, true, win, named["DSVS_PASTE"],
		xproto.GetPropertyTypeAny, 0, 1<<16).Reply()
	if err != nil {
		t.Fatal(err)
	}
	return string(reply.Value)
}
