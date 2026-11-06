package core

import (
	"strings"
	"testing"
)

func TestJournalWritesLikeATerminal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		feed  []string
		lines []string
	}{{
		name:  "the spinner rewrites itself in place",
		feed:  []string{"❯ Building...\n", "\x1b[1A\x1b[2K✔ Building done\n"},
		lines: []string{"✔ Building done", ""},
	}, {
		name:  "the progress bar goes back to the start of the line",
		feed:  []string{"loading  50%\rloading 100%"},
		lines: []string{"loading 100%"},
	}, {
		name:  "a carriage return only erases what it covers",
		feed:  []string{"100%\r50%"},
		lines: []string{"50%%"},
	}, {
		name:  "erase to the end of the line",
		feed:  []string{"abcdef", "\rab\x1b[K"},
		lines: []string{"ab"},
	}, {
		name:  "erase n characters",
		feed:  []string{"abcdef", "\r\x1b[3X"},
		lines: []string{"   def"},
	}, {
		name:  "delete and insert characters",
		feed:  []string{"abcdef\r\x1b[2P"},
		lines: []string{"cdef"},
	}, {
		name:  "move up then down",
		feed:  []string{"one\ntwo\nthree\n", "\x1b[2A\x1b[2Ktwo again\x1b[2B\r\x1b[2Kend"},
		lines: []string{"one", "two again", "three", "end"},
	}, {
		name:  "absolute columns",
		feed:  []string{"abcdef\x1b[3Gxy"},
		lines: []string{"abxyef"},
	}, {
		name:  "delete a line",
		feed:  []string{"one\ntwo\nthree\n\x1b[3A\x1b[1M"},
		lines: []string{"two", "three", ""},
	}, {
		name:  "absolute positioning counts from the top of the screen",
		feed:  []string{"one\ntwo\nthree\n", "\x1b[2;3Hxx"},
		lines: []string{"one", "twxx", "three", ""},
	}, {
		name:  "tabs advance by eight",
		feed:  []string{"ab\tc"},
		lines: []string{"ab      c"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := NewJournal("app", 100, 40)
			for _, chunk := range c.feed {
				j.Write([]byte(chunk))
			}
			got := texts(j.Lines())
			if len(got) != len(c.lines) {
				t.Fatalf("%d lines, want %d: %q", len(got), len(c.lines), got)
			}
			for i := range got {
				if got[i] != c.lines[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], c.lines[i])
				}
			}
		})
	}
}

// Colors survive, and a partial rewrite leaves no bleeding style.
func TestJournalKeepsStyles(t *testing.T) {
	t.Parallel()
	j := NewJournal("app", 10, 40)
	j.Write([]byte("\x1b[32mgreen\x1b[0m plain"))

	line := j.Lines()[0].Text
	if !strings.Contains(line, "\x1b[32m") || !strings.Contains(line, "green") {
		t.Fatalf("the color is lost: %q", line)
	}
	if strings.Count(line, "\x1b[0m") != 1 || !strings.HasSuffix(line, "plain") {
		t.Errorf("the style should close once, before the plain text: %q", line)
	}

	// Rewriting over it in a neutral style must not leave the color behind.
	j.Write([]byte("\rblack"))
	if line := j.Lines()[0].Text; strings.Contains(line, "\x1b[32m") {
		t.Errorf("the color survived its rewrite: %q", line)
	}
}

// A sequence or a rune cut between two terminal reads must be stitched back together.
func TestJournalHandlesSplitWrites(t *testing.T) {
	t.Parallel()
	j := NewJournal("app", 10, 40)
	for _, chunk := range []string{"\x1b[3", "2mgreen\x1b[0m ", "caf\xc3", "\xa9"} {
		j.Write([]byte(chunk))
	}

	line := j.Lines()[0].Text
	if !strings.Contains(line, "\x1b[32m") {
		t.Errorf("the cut sequence was not stitched back: %q", line)
	}
	if !strings.Contains(line, "café") {
		t.Errorf("the cut rune was not stitched back: %q", line)
	}
}

// Plain mode only writes the lines the cursor has left: giving it the current line would force it
// to come back to it.
func TestJournalOnlyReportsClosedLines(t *testing.T) {
	t.Parallel()
	j := NewJournal("app", 10, 40)
	j.Write([]byte("close\nongoing"))

	lines, next, lost := j.Since(0)
	if len(lines) != 1 || lines[0].Text != "close" || lost != 0 {
		t.Fatalf("Since(0) = %q, next=%d, lost=%d", texts(lines), next, lost)
	}
	if lines, _, _ := j.Since(next); len(lines) != 0 {
		t.Fatalf("nothing new was expected, returned %q", texts(lines))
	}

	j.Write([]byte(" done\nnext"))
	if lines, _, _ := j.Since(next); len(lines) != 1 || lines[0].Text != "ongoing done" {
		t.Errorf("the completed line should come out whole, returned %q", texts(lines))
	}
}

func TestJournalForgetsOldestAndSaysSo(t *testing.T) {
	t.Parallel()
	j := NewJournal("app", 3, 40)
	j.Write([]byte("1\n2\n3\n4\n5"))

	got := texts(j.Lines())
	if len(got) != 3 || got[0] != "3" {
		t.Fatalf("the log should keep the last three lines, returned %q", got)
	}
	lines, _, lost := j.Since(0)
	if lost != 2 {
		t.Errorf("%d lost lines announced, want 2 (%q)", lost, texts(lines))
	}
}

// The supervisor's lines do not mix with a line being written.
func TestJournalAddStartsItsOwnLine(t *testing.T) {
	t.Parallel()
	j := NewJournal("app", 10, 40)
	j.Write([]byte("output without a line end"))
	j.Add("-- exited (1)")

	got := texts(j.Lines())
	if len(got) != 3 || got[0] != "output without a line end" || got[1] != "-- exited (1)" {
		t.Fatalf("lines = %q", got)
	}
}

func TestMergeOrdersByTime(t *testing.T) {
	t.Parallel()
	a, b := NewJournal("a", 4, 40), NewJournal("b", 4, 40)
	a.Add("a1")
	b.Add("b1")
	a.Add("a2")

	var got []string
	for _, line := range Merge(a.Lines(), b.Lines()) {
		if line.Text != "" {
			got = append(got, line.Src+":"+line.Text)
		}
	}
	want := []string{"a:a1", "b:b1", "a:a2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("merge = %v, want %v", got, want)
		}
	}
}

func texts(lines []Line) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, line.Text)
	}
	return out
}

// Styles are decoded, not stacked: a back-to-default cancels, it does not add.
func TestJournalNormalisesStyles(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"\x1b[36m\x1b[39mplain":                "plain",
		"\x1b[1m\x1b[31mstrong\x1b[0m":         "\x1b[1;31mstrong\x1b[0m",
		"\x1b[2m\x1b[22mplain":                 "plain",
		"\x1b[38;5;208morange\x1b[39m":         "\x1b[38;5;208morange\x1b[0m",
		"\x1b[4munderlined\x1b[24m then plain": "\x1b[4munderlined\x1b[0m then plain",
		"\x1b[31;1mbold red\x1b[0m":            "\x1b[1;31mbold red\x1b[0m",
	}
	for input, want := range cases {
		j := NewJournal("app", 10, 40)
		j.Write([]byte(input))
		if got := j.Lines()[0].Text; got != want {
			t.Errorf("%q returned %q, want %q", input, got, want)
		}
	}

	// An avalanche of sequences does not grow the style they describe.
	j := NewJournal("app", 10, 40)
	for range 200 {
		j.Write([]byte("\x1b[36m\x1b[39m\x1b[2m\x1b[22m"))
	}
	j.Write([]byte("x"))
	if got := j.Lines()[0].Text; got != "x" {
		t.Errorf("the style bled: %q", got)
	}
}

// The screen is the window of the last lines, the one the app believes it has: it locates the
// top-left corner, and what scrolled above is no longer part of it.
func TestJournalScreenIsAWindow(t *testing.T) {
	t.Parallel()
	j := NewJournal("app", 100, 3)
	j.Write([]byte("one\ntwo\nthree\nfour\nfive"))

	// The cursor is on "five" (line 4): the three-line window covers three, four, five.
	j.Write([]byte("\x1b[1;1HTHREE"))
	if got := texts(j.Lines()); got[2] != "THREE" || got[0] != "one" {
		t.Fatalf("the top-left corner should target the window, returned %q", got)
	}

	// Clearing the screen removes its lines; what scrolled before stays.
	j.Write([]byte("\x1b[2J"))
	if got := texts(j.Lines()); len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "" {
		t.Fatalf("after a screen clear: %q", got)
	}

	// The screen follows what it is told of its height.
	j.SetHeight(1)
	j.Write([]byte("alone\n\x1b[1;1Hreplaced"))
	if got := texts(j.Lines()); got[len(got)-1] != "replaced" {
		t.Errorf("with a one-line screen, the top-left corner is the last line: %q", got)
	}
}

// Erase from the cursor to the end of the screen, and from the start of the screen to the cursor.
func TestJournalErasesPartsOfTheScreen(t *testing.T) {
	t.Parallel()
	forward := NewJournal("app", 100, 10)
	forward.Write([]byte("one\ntwo\nthree\n\x1b[2A\x1b[0J"))
	if got := texts(forward.Lines()); len(got) != 4 || got[0] != "one" || got[1] != "" || got[2] != "" {
		t.Errorf("erase down = %q", got)
	}

	backward := NewJournal("app", 100, 10)
	backward.Write([]byte("one\ntwo\nthree\n\x1b[1A\x1b[1J"))
	if got := texts(backward.Lines()); got[0] != "" || got[1] != "" || got[3] != "" {
		t.Errorf("erase up = %q", got)
	}
}

// A log is replayed through the path that writes it: its dump read back gives the same lines,
// styles and in-place rewrites included. It is what carries the logs across a handover.
func TestDumpReplays(t *testing.T) {
	log := NewJournal("app", 4096, 4)
	log.Write([]byte("\x1b[32mgreen\x1b[0m and \x1b[1mbold\x1b[0m\r\n"))
	log.Write([]byte("loading\rdone\r\n"))
	log.Add("-- a supervisor line")
	log.Write([]byte("no final newline"))

	replayed := NewJournal("app", 4096, 4)
	replayed.Write([]byte(log.Dump()))

	want, got := lines(log), lines(replayed)
	if len(want) != len(got) {
		t.Fatalf("replayed lines = %d, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("replayed line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if row, col := replayed.Cursor(); row != len(want)-1 || col == 0 {
		t.Errorf("replayed cursor = %d,%d, want the end of the last line", row, col)
	}
}

func lines(log *Journal) []string {
	out := make([]string, 0, 8)
	for _, line := range log.Lines() {
		out = append(out, line.Text)
	}
	return out
}
