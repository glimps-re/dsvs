package tray

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"dsvs/internal/attach"
	"dsvs/internal/ui/panel"
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
// name has them violet. Two icons in the same status bar therefore tell apart without hovering.
func TestTheBarsTellWhichBinaryPutTheIcon(t *testing.T) {
	const white, violet = 0xf0f0f0, 0xb9a3ff
	if got := tint("dsvs", true); got != rgb(white) {
		t.Errorf("the working dsvs paints its bars %v, want white", got)
	}
	for _, name := range []string{"dsvs-beta", "dsvs-daemon", "dsvs2", ""} {
		if got := tint(name, true); got != rgb(violet) {
			t.Errorf("%q paints its bars %v, want violet", name, got)
		}
	}
}

var moods = []panel.Mood{panel.Idle, panel.Running, panel.Starting, panel.Failed}

// The icon is drawn at the size the bar imposes, and the bars follow its dimensions.
func TestTheBarsFollowTheSizeOfTheIcon(t *testing.T) {
	for size, tops := range map[int][]int{16: {2, 6, 10}, 22: {3, 9, 15}, 24: {3, 9, 15}, 32: {5, 14, 23}} {
		for row, bar := range bars(size) {
			if bar.Min.Y != tops[row] {
				t.Errorf("%d px: bar %d starts at y=%d, want %d", size, row, bar.Min.Y, tops[row])
			}
		}
	}
}

// Every pixel is painted — the status bar gives no background, and an unpainted pixel comes out white.
func TestEveryPixelOfTheIconIsPainted(t *testing.T) {
	for _, size := range []int{16, 22, 24, 32} {
		for _, mood := range moods {
			img := render(size, mood, "dsvs", 0)
			if img.Bounds().Dx() != size || img.Bounds().Dy() != size {
				t.Fatalf("%d px: image of %v", size, img.Bounds())
			}
			for x := 3; x < len(img.Pix); x += 4 {
				if img.Pix[x] != 0xff {
					t.Fatalf("%d px, %v: pixel %d translucent", size, mood, x/4)
				}
			}
		}
	}
}

// Under a Qubes tint or with deuteranopia, only luminance survives: the four states must differ
// pairwise in it, on at least a tenth of the pixels.
func TestTheFourMoodsDifferInLuminanceAlone(t *testing.T) {
	for _, size := range []int{16, 22, 24, 32} {
		for _, name := range []string{"dsvs", "dsvs-beta"} {
			for i, a := range moods {
				for _, b := range moods[i+1:] {
					if share := differing(render(size, a, name, 0), render(size, b, name, 0)); share < 0.10 {
						t.Errorf("%d px, %s: %v and %v differ on only %.1f%% of the pixels", size, name, a, b, 100*share)
					}
				}
			}
		}
	}
}

// When nothing runs, the badge's corner holds only the tile and the bars; as soon as an app runs, all
// being well, it holds a badge, and a still one.
func TestABadgeSaysSomethingRuns(t *testing.T) {
	for _, size := range []int{16, 22, 24, 32} {
		for mood, want := range map[panel.Mood]bool{panel.Idle: false, panel.Running: true} {
			img, badged := render(size, mood, "dsvs", 0), false
			for y := size / 2; y < size; y++ {
				for x := size / 2; x < size; x++ {
					badged = badged || (!underABar(size, x, y) && img.At(x, y) != tile)
				}
			}
			if badged != want {
				t.Errorf("%d px, %v: badge = %v, want %v", size, mood, badged, want)
			}
		}
	}
}

// The bars tell the binary, not the state: the same state reads differently from one binary to the
// other, and the badge stays the state's.
func TestTheBinaryShowsInTheBarsAtEqualMood(t *testing.T) {
	for _, mood := range moods {
		if differing(render(22, mood, "dsvs", 0), render(22, mood, "dsvs-beta", 0)) == 0 {
			t.Errorf("%v: dsvs and another binary have the same icon", mood)
		}
	}
}

// Only "starting" spins: the animation frames differ in that state alone.
func TestOnlyStartingTurns(t *testing.T) {
	for _, mood := range moods {
		turns := differing(render(22, mood, "dsvs", 0), render(22, mood, "dsvs", 1)) > 0
		if turns != (mood == panel.Starting) {
			t.Errorf("%v: spins = %v", mood, turns)
		}
	}
	if !reflect.DeepEqual(render(22, panel.Starting, "dsvs", 0), render(22, panel.Starting, "dsvs", 4)) {
		t.Error("after four quarter turns, the badge did not come back")
	}
}

// TestLookLeavesTheIconAsPNGs drops the enlarged icon into the folder named by LOOK: the four states
// of both binaries at 22 px, their four sizes, and the four animation frames.
func TestLookLeavesTheIconAsPNGs(t *testing.T) {
	into := os.Getenv("LOOK")
	if into == "" {
		t.Skip("LOOK=<folder> drops the rendering as PNG")
	}
	const zoom, gap = 8, 12
	sheet := func(name string, rows [][]*image.RGBA) {
		var w, h int
		for _, row := range rows {
			rowW, rowH := gap, 0
			for _, icon := range row {
				rowW += icon.Bounds().Dx()*zoom + gap
				rowH = max(rowH, icon.Bounds().Dy()*zoom)
			}
			w, h = max(w, rowW), h+rowH+gap
		}
		out := image.NewRGBA(image.Rect(0, 0, w, h+gap))
		draw.Draw(out, out.Bounds(), image.NewUniform(color.RGBA{0x2b, 0x2b, 0x2e, 0xff}), image.Point{}, draw.Src)
		y := gap
		for _, row := range rows {
			x, rowH := gap, 0
			for _, icon := range row {
				for py := range icon.Bounds().Dy() * zoom {
					for px := range icon.Bounds().Dx() * zoom {
						out.Set(x+px, y+py, icon.At(px/zoom, py/zoom))
					}
				}
				x, rowH = x+icon.Bounds().Dx()*zoom+gap, max(rowH, icon.Bounds().Dy()*zoom)
			}
			y += rowH + gap
		}
		file, err := os.Create(filepath.Join(into, name))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := png.Encode(file, out); err != nil {
			t.Fatal(err)
		}
	}

	var states, sizes, frames [][]*image.RGBA
	for _, name := range []string{"dsvs", "dsvs-beta"} {
		var at22, bySize, turning []*image.RGBA
		for _, mood := range moods {
			at22 = append(at22, render(22, mood, name, 0))
		}
		for _, size := range []int{16, 22, 24, 32} {
			bySize = append(bySize, render(size, panel.Failed, name, 0))
		}
		for frame := range 4 {
			turning = append(turning, render(22, panel.Starting, name, frame))
		}
		states, sizes, frames = append(states, at22), append(sizes, bySize), append(frames, turning)
	}
	sheet("icon-states.png", states)
	sheet("icon-sizes.png", sizes)
	sheet("icon-animation.png", frames)
}

// differing is the share of pixels whose luminance differs.
func differing(a, b *image.RGBA) float64 {
	count := 0
	for i := 0; i < len(a.Pix); i += 4 {
		if math.Abs(luma(a.Pix[i:])-luma(b.Pix[i:])) > 8 {
			count++
		}
	}
	return float64(count) / float64(len(a.Pix)/4)
}

func luma(px []uint8) float64 {
	return 0.2126*float64(px[0]) + 0.7152*float64(px[1]) + 0.0722*float64(px[2])
}

func underABar(size, x, y int) bool {
	return slices.ContainsFunc(bars(size), func(bar image.Rectangle) bool { return image.Pt(x, y).In(bar) })
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

// The icon redraws only when what the sessions say together changes: twenty apps starting, across as
// many sessions or messages, make a single drawing.
func TestTwentyIdenticalStatesRedrawOnce(t *testing.T) {
	var drawn []panel.Mood
	tally := &tally{said: func(mood panel.Mood) { drawn = append(drawn, mood) }, states: map[string]attach.State{}}
	booting := attach.State{Apps: []attach.App{{Name: "api", Status: "starting"}}}

	for i := range 20 {
		tally.see(fmt.Sprintf("/work/%d/dsvs.json", i%5), booting)
	}
	if !slices.Equal(drawn, []panel.Mood{panel.Starting}) {
		t.Fatalf("twenty identical states drew %v, want a single Starting", drawn)
	}

	for config := range tally.states {
		tally.drop(config)
	}
	if !slices.Equal(drawn, []panel.Mood{panel.Starting, panel.Idle}) {
		t.Fatalf("the departed sessions drew %v, want a return to Idle", drawn)
	}
}

// No timer outside "starting": a calm or fallen icon repaints only when a state changes.
func TestOnlyStartingAsksForATimer(t *testing.T) {
	for mood, want := range map[panel.Mood]bool{
		panel.Idle: false, panel.Running: false, panel.Starting: true, panel.Failed: false,
	} {
		if got := turns(mood); got != want {
			t.Errorf("%v: timer %v, want %v", mood, got, want)
		}
	}
}
