package core

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Line is a log line, dated and attributed to its source so that a merged log stays ordered and
// readable.
type Line struct {
	At   time.Time
	Src  string
	Text string
}

// Journal is a source's virtual terminal. The bytes it is given are written to it as on a screen —
// the cursor moves, erasures erase, styles hold — except that what scrolls off the top is kept
// instead of lost: the log is the screen and its history in one piece. That is what lets progress
// bars and spinners rewrite themselves in place instead of stacking lines.
//
// The screen is the window of the last `rows` lines, the one the app believes it has. It gives
// absolute positioning its meaning: "top-left corner" means the top of that window, not the start
// of the log.
type Journal struct {
	src string
	max int

	mu      sync.Mutex
	lines   []*line
	row     int // cursor line, in lines
	col     int // cursor column, in cells
	top     int // first line of the screen, in lines
	rows    int // height of the screen lent to the app
	attrs   attrs
	pen     string
	pens    map[string]string // the styles met, shared between cells
	dropped int               // lines forgotten off the top
	rest    string
}

// attrs is the current style, decoded: accumulating the received sequences would grow them
// endlessly, since a "back to default" (39, 49, 22…) is one more sequence.
type attrs struct {
	fg, bg    string
	bold      bool
	faint     bool
	italic    bool
	underline bool
	reverse   bool
	strike    bool
}

// sequence returns the style in its shortest form, or nothing if it is neutral.
func (a attrs) sequence() string {
	var codes []string
	for _, flag := range []struct {
		on   bool
		code string
	}{{a.bold, "1"}, {a.faint, "2"}, {a.italic, "3"}, {a.underline, "4"}, {a.reverse, "7"}, {a.strike, "9"}} {
		if flag.on {
			codes = append(codes, flag.code)
		}
	}
	if a.fg != "" {
		codes = append(codes, a.fg)
	}
	if a.bg != "" {
		codes = append(codes, a.bg)
	}
	if codes == nil {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// apply consumes the parameters of an SGR.
func (a *attrs) apply(body string) {
	params := strings.Split(body, ";")
	for i := 0; i < len(params); i++ {
		switch code := number(params[i], 0); code {
		case 0:
			*a = attrs{}
		case 1:
			a.bold = true
		case 2:
			a.faint = true
		case 3:
			a.italic = true
		case 4:
			a.underline = true
		case 7:
			a.reverse = true
		case 9:
			a.strike = true
		case 21, 22:
			a.bold, a.faint = false, false
		case 23:
			a.italic = false
		case 24:
			a.underline = false
		case 27:
			a.reverse = false
		case 29:
			a.strike = false
		case 39:
			a.fg = ""
		case 49:
			a.bg = ""
		case 38, 48:
			// Extended color: 5;n for the palette, 2;r;g;b for the true tone.
			extended, used := extended(params[i:])
			if code == 38 {
				a.fg = extended
			} else {
				a.bg = extended
			}
			i += used
		default:
			switch {
			case code >= 30 && code <= 37, code >= 90 && code <= 97:
				a.fg = params[i]
			case code >= 40 && code <= 47, code >= 100 && code <= 107:
				a.bg = params[i]
			}
		}
	}
}

// extended reads a 256 or 24-bit color and says how many parameters it consumed.
func extended(params []string) (string, int) {
	if len(params) < 2 {
		return "", 0
	}
	switch kind := number(params[1], 0); {
	case kind == 5 && len(params) >= 3:
		return strings.Join(params[:3], ";"), 2
	case kind == 2 && len(params) >= 5:
		return strings.Join(params[:5], ";"), 4
	}
	return "", 0
}

type line struct {
	at     time.Time
	cells  []cell
	text   string
	cached bool
}

type cell struct {
	r   rune
	pen string
}

func NewJournal(src string, size, rows int) *Journal {
	return &Journal{src: src, max: size, rows: max(1, rows), lines: []*line{{at: time.Now()}}}
}

// Name is the source whose lines this log carries: the interfaces display it, nobody else needs to
// know it.
func (j *Journal) Name() string { return j.src }

// SetHeight tells the log the height of the screen the app has; it is what locates the top-left
// corner.
func (j *Journal) SetHeight(rows int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.rows = rows
	j.follow()
}

// Write makes Journal an io.Writer: an app's pty pours straight into it.
func (j *Journal) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.feed(string(p))
	return len(p), nil
}

// Add writes a supervisor line, always alone on its line.
func (j *Journal) Add(text string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.col > 0 {
		j.newline()
	}
	j.feed(text + "\r\n")
}

func (j *Journal) Addf(format string, args ...any) {
	j.Add(fmt.Sprintf(format, args...))
}

// Lines returns everything the log keeps, from the oldest line to the one the cursor writes on.
func (j *Journal) Lines() []Line {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Line, 0, len(j.lines))
	for _, l := range j.lines {
		out = append(out, Line{At: l.at, Src: j.src, Text: j.render(l)})
	}
	return out
}

// Cursor returns the position where the app writes: the line, among those Lines returns, and the
// column. An interface uses it to put the cursor where it really is.
func (j *Journal) Cursor() (row, col int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.row, j.col
}

// Since returns the lines completed since seq — those the cursor has left, hence the only ones that
// can be written to a stream without having to take them back — and how many were forgotten in the
// meantime.
func (j *Journal) Since(seq int) (lines []Line, next, lost int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	closed := j.dropped + j.row
	from := max(seq, j.dropped)
	if from >= closed {
		return nil, closed, max(0, j.dropped-seq)
	}
	for _, l := range j.lines[from-j.dropped : closed-j.dropped] {
		lines = append(lines, Line{At: l.at, Src: j.src, Text: j.render(l)})
	}
	return lines, closed, max(0, j.dropped-seq)
}

// Merge interleaves already ordered series into one, by date.
func Merge(series ...[]Line) []Line {
	total := 0
	for _, s := range series {
		total += len(s)
	}
	out, heads := make([]Line, 0, total), make([]int, len(series))
	for range total {
		oldest := -1
		for i, s := range series {
			if heads[i] < len(s) && (oldest < 0 || s[heads[i]].At.Before(series[oldest][heads[oldest]].At)) {
				oldest = i
			}
		}
		out = append(out, series[oldest][heads[oldest]])
		heads[oldest]++
	}
	return out
}

// feed applies a chunk of the stream. A sequence cut at the end of a chunk is kept for the next
// one.
func (j *Journal) feed(chunk string) {
	text := j.rest + chunk
	j.rest = ""
	for i := 0; i < len(text); {
		switch c := text[i]; {
		case c == '\n':
			j.newline()
			i++
		case c == '\r':
			j.col = 0
			i++
		case c == '\b':
			j.col = max(0, j.col-1)
			i++
		case c == '\t':
			j.col += 8 - j.col%8
			i++
		case c == escape:
			sequence, next, complete := readEscape(text, i)
			if !complete {
				j.rest = text[i:]
				return
			}
			j.control(sequence)
			i = next
		case c < ' ':
			i++ // the other control characters make no sense in a log
		default:
			r, size := decodeRune(text[i:])
			if size == 0 {
				j.rest = text[i:] // rune cut between two chunks
				return
			}
			j.put(r)
			i += size
		}
	}
}

func (j *Journal) control(sequence string) {
	if !strings.HasPrefix(sequence, "\x1b[") {
		return // two-byte sequences do not concern the content
	}
	body := sequence[2 : len(sequence)-1]
	verb := sequence[len(sequence)-1]
	if strings.HasPrefix(body, "?") {
		return // private modes (cursor visible, mouse…): nothing to write
	}
	n := number(body, 1)
	switch verb {
	case 'm':
		j.style(body)
	case 'A':
		j.row = max(0, j.row-n)
		j.clampColumn()
	case 'B':
		for range n {
			j.down()
		}
	case 'C':
		j.col += n
	case 'D':
		j.col = max(0, j.col-n)
	case 'E':
		for range n {
			j.down()
		}
		j.col = 0
	case 'F':
		j.row = max(0, j.row-n)
		j.col = 0
	case 'G', '`':
		j.col = max(0, n-1)
	case 'H', 'f':
		j.moveTo(body)
	case 'd':
		j.row = min(j.top+max(0, n-1), j.top+j.rows-1)
		j.current()
	case 'K':
		j.eraseInLine(number(body, 0))
	case 'J':
		j.eraseInScreen(number(body, 0))
	case 'X':
		j.blank(j.col, j.col+n)
	case 'P':
		j.delete(n)
	case '@':
		j.insert(n)
	case 'L':
		j.insertLines(n)
	case 'M':
		j.deleteLines(n)
	}
}

// style decodes an SGR and keeps the resulting style, shared between all the cells that carry it.
func (j *Journal) style(body string) {
	if body == "" {
		body = "0"
	}
	j.attrs.apply(body)
	sequence := j.attrs.sequence()
	if shared, seen := j.pens[sequence]; seen {
		j.pen = shared
		return
	}
	if j.pens == nil {
		j.pens = map[string]string{}
	}
	j.pens[sequence] = sequence
	j.pen = sequence
}

func (j *Journal) put(r rune) {
	l := j.current()
	if len(l.cells) == 0 {
		// A line is dated from its first character: born when the previous one completes, it can
		// wait a long time to be written to, and a merged log would sort it too early.
		l.at = time.Now()
	}
	for len(l.cells) <= j.col {
		l.cells = append(l.cells, cell{r: ' '})
	}
	l.cells[j.col] = cell{r: r, pen: j.pen}
	l.cached = false
	j.col++
}

// moveTo places the cursor in the screen, line then column, counted from its top-left corner.
func (j *Journal) moveTo(body string) {
	row, col := 1, 1
	if before, after, found := strings.Cut(body, ";"); found {
		row, col = number(before, 1), number(after, 1)
	} else {
		row = number(body, 1)
	}
	j.row = min(j.top+max(0, row-1), j.top+j.rows-1)
	j.col = max(0, col-1)
	j.current()
}

func (j *Journal) newline() {
	j.down()
	j.col = 0
}

func (j *Journal) down() {
	j.row++
	for len(j.lines) <= j.row {
		j.lines = append(j.lines, &line{at: time.Now()})
	}
	j.follow()
	j.forget()
}

// follow scrolls the screen down when the cursor goes below its bottom.
func (j *Journal) follow() {
	if j.row > j.top+j.rows-1 {
		j.top = j.row - j.rows + 1
	}
	j.top = max(0, min(j.top, len(j.lines)-1))
}

// forget forgets the oldest lines when the log overflows.
func (j *Journal) forget() {
	if len(j.lines) <= j.max {
		return
	}
	extra := len(j.lines) - j.max
	j.lines = j.lines[extra:]
	j.dropped += extra
	j.row -= extra
	j.top = max(0, j.top-extra)
}

func (j *Journal) current() *line {
	for len(j.lines) <= j.row {
		j.lines = append(j.lines, &line{at: time.Now()})
	}
	return j.lines[j.row]
}

func (j *Journal) clampColumn() {
	if l := j.current(); j.col > len(l.cells) {
		j.col = len(l.cells)
	}
}

func (j *Journal) eraseInLine(mode int) {
	l := j.current()
	switch mode {
	case 0:
		if j.col < len(l.cells) {
			l.cells = l.cells[:j.col]
		}
	case 1:
		j.blank(0, min(j.col+1, len(l.cells)))
	case 2:
		l.cells = l.cells[:0]
	}
	l.cached = false
}

// eraseInScreen erases within the window. Clearing the whole screen removes its lines instead of
// leaving as many blank lines there; what already scrolled above stays.
func (j *Journal) eraseInScreen(mode int) {
	bottom := min(j.top+j.rows-1, len(j.lines)-1)
	switch mode {
	case 0:
		j.eraseInLine(0)
		j.clearLines(j.row+1, bottom)
	case 1:
		j.clearLines(j.top, j.row-1)
		j.eraseInLine(1)
	default:
		// The screen is empty, and the cursor is in it: it needs a line to write on.
		j.lines = append(j.lines[:j.top], &line{at: time.Now()})
		j.row, j.col, j.top = len(j.lines)-1, 0, len(j.lines)-1
	}
}

func (j *Journal) clearLines(from, to int) {
	for r := max(0, from); r <= min(to, len(j.lines)-1); r++ {
		j.lines[r].cells, j.lines[r].cached = nil, false
	}
}

func (j *Journal) blank(from, to int) {
	l := j.current()
	for i := from; i < min(to, len(l.cells)); i++ {
		l.cells[i] = cell{r: ' '}
	}
	l.cached = false
}

func (j *Journal) delete(n int) {
	l := j.current()
	if j.col >= len(l.cells) {
		return
	}
	l.cells = append(l.cells[:j.col], l.cells[min(j.col+n, len(l.cells)):]...)
	l.cached = false
}

func (j *Journal) insert(n int) {
	l := j.current()
	for len(l.cells) < j.col {
		l.cells = append(l.cells, cell{r: ' '})
	}
	blanks := make([]cell, n)
	for i := range blanks {
		blanks[i] = cell{r: ' '}
	}
	l.cells = append(l.cells[:j.col], append(blanks, l.cells[j.col:]...)...)
	l.cached = false
}

func (j *Journal) insertLines(n int) {
	fresh := make([]*line, n)
	for i := range fresh {
		fresh[i] = &line{at: time.Now()}
	}
	j.lines = append(j.lines[:j.row], append(fresh, j.lines[j.row:]...)...)
	j.forget()
}

func (j *Journal) deleteLines(n int) {
	end := min(j.row+n, len(j.lines))
	j.lines = append(j.lines[:j.row], j.lines[end:]...)
	if len(j.lines) == 0 {
		j.lines = []*line{{at: time.Now()}}
	}
	j.row = min(j.row, len(j.lines)-1)
}

// render assembles a line, grouping cells of the same style.
func (j *Journal) render(l *line) string {
	if l.cached {
		return l.text
	}
	var out strings.Builder
	pen := ""
	for _, c := range l.cells {
		if c.pen != pen {
			if pen != "" {
				out.WriteString("\x1b[0m")
			}
			out.WriteString(c.pen)
			pen = c.pen
		}
		out.WriteRune(c.r)
	}
	if pen != "" {
		out.WriteString("\x1b[0m")
	}
	l.text, l.cached = strings.TrimRight(out.String(), " "), true
	return l.text
}

const escape = '\x1b'

// readEscape returns the sequence starting at i, where the text resumes, and whether it is
// complete.
func readEscape(text string, i int) (sequence string, next int, complete bool) {
	if i+1 >= len(text) {
		return "", i, false
	}
	switch text[i+1] {
	case '[':
		for k := i + 2; k < len(text); k++ {
			if text[k] >= 0x40 && text[k] <= 0x7E {
				return text[i : k+1], k + 1, true
			}
		}
		return "", i, false
	case ']':
		for k := i + 2; k < len(text); k++ {
			if text[k] == 0x07 {
				return text[i : k+1], k + 1, true
			}
			if text[k] == escape && k+1 < len(text) && text[k+1] == '\\' {
				return text[i : k+2], k + 2, true
			}
		}
		return "", i, false
	default:
		// A sequence can carry intermediate bytes before its final byte, like \x1b(B.
		k := i + 1
		for ; k < len(text) && text[k] >= 0x20 && text[k] <= 0x2F; k++ {
		}
		if k >= len(text) {
			return "", i, false
		}
		return text[i : k+1], k + 1, true
	}
}

func number(body string, fallback int) int {
	head, _, _ := strings.Cut(body, ";")
	n := 0
	for _, c := range head {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	if head == "" {
		return fallback
	}
	return n
}

// decodeRune returns the leading rune and its size, or 0 if it is cut.
func decodeRune(s string) (rune, int) {
	size := 1
	switch {
	case s[0] >= 0xF0:
		size = 4
	case s[0] >= 0xE0:
		size = 3
	case s[0] >= 0xC0:
		size = 2
	}
	if len(s) < size {
		return 0, 0
	}
	return []rune(s[:size])[0], size
}

// Dump returns the log as a terminal would have written it: its rendered lines, separated by line
// breaks. Write eats it back, so a log is replayed through the path that normally writes it. The
// lines' timestamps, however, do not cross over: a replayed log is dated from its replay.
func (j *Journal) Dump() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	texts := make([]string, 0, len(j.lines))
	for _, l := range j.lines {
		texts = append(texts, j.render(l))
	}
	return strings.Join(texts, "\r\n")
}
