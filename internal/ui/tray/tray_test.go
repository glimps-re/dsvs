package tray

import (
	"image"
	"testing"
	"time"
)

// A click is a round trip: releasing over the icon is what acts, and an abandoned gesture opens
// nothing. That shields us from a window manager that grabs the press to give focus — measured, it
// then took two clicks. What the icon passes is its own rectangle, not the pointer's position: the
// panel decides which side to open on.
func TestOnlyAClickReleasedOnTheIconActs(t *testing.T) {
	opened := make(chan image.Rectangle, 4)
	ended := make(chan struct{}, 4)
	open := func(anchor image.Rectangle, until <-chan struct{}, gone func()) error {
		opened <- anchor
		<-until
		gone()
		ended <- struct{}{}
		return nil
	}
	i := &icon{size: 22}
	// The icon's top-left corner is at (100, 178): the pointer is at (103, 183) on screen, so (3, 5)
	// in the icon. The expected anchor is therefore its 22 px square from (100, 178).
	root, where := image.Pt(103, 183), image.Pt(3, 5)
	anchor := image.Rect(100, 178, 122, 200)

	i.release(open, image.Pt(3, 3))
	if came(opened) {
		t.Fatal("a release without a press opened the panel")
	}

	i.press(root, where)
	i.release(open, image.Pt(30, 30))
	if came(opened) {
		t.Fatal("a click released outside the icon opened the panel")
	}

	i.press(root, where)
	i.release(open, image.Pt(3, 3))
	if at := waitFor(t, opened); at != anchor {
		t.Fatalf("panel anchored at %v, want the icon's rectangle %v", at, anchor)
	}

	i.press(root, where)
	if came(ended) {
		t.Fatal("the panel closed on the press")
	}
	i.release(open, image.Pt(3, 3))
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("the second click did not remove the panel")
	}
	if came(opened) {
		t.Fatal("the second click placed a panel again instead of removing the first")
	}
}

// Once the window is gone, the icon reopens on the next click without waiting for what the panel
// started: an update runs to completion, but it no longer swallows the click of whoever watches it.
func TestAClickReopensAsSoonAsTheWindowIsGone(t *testing.T) {
	opened := make(chan image.Rectangle, 4)
	working := make(chan struct{})
	open := func(anchor image.Rectangle, until <-chan struct{}, gone func()) error {
		gone()
		opened <- anchor // announced after gone: the next click finds the icon free
		<-working        // the panel is gone, its work goes on
		return nil
	}
	defer close(working)
	i := &icon{size: 22}

	i.press(image.Pt(13, 23), image.Pt(3, 3))
	i.release(open, image.Pt(3, 3))
	waitFor(t, opened)

	// The icon moved: the second panel anchors on its new square, from (40, 38).
	i.press(image.Pt(45, 44), image.Pt(5, 6))
	i.release(open, image.Pt(3, 3))
	if at := waitFor(t, opened); at != image.Rect(40, 38, 62, 60) {
		t.Fatalf("second panel anchored at %v, want the icon's new rectangle", at)
	}
}

// The bar tint says which binary placed the icon: only the working dsvs has them white, and any other
// name has them amber. Two icons in the same status bar therefore tell apart without hovering.
func TestTheBarsTellWhichBinaryPutTheIcon(t *testing.T) {
	const white, amber = 0xf0f0f0, 0xf0b040
	if got := tint("dsvs"); got != white {
		t.Errorf("the working dsvs paints its bars %#06x, want white %#06x", got, white)
	}
	for _, name := range []string{"dsvs-beta", "dsvs-daemon", "dsvs2", ""} {
		if got := tint(name); got != amber {
			t.Errorf("%q paints its bars %#06x, want amber %#06x", name, got, amber)
		}
	}
}

func waitFor(t *testing.T, from chan image.Rectangle) image.Rectangle {
	t.Helper()
	select {
	case at := <-from:
		return at
	case <-time.After(time.Second):
		t.Fatal("the panel did not open")
		return image.Rectangle{}
	}
}

// came gives what was due the time to arrive, and says whether something came.
func came[T any](from chan T) bool {
	select {
	case <-from:
		return true
	case <-time.After(150 * time.Millisecond):
		return false
	}
}
