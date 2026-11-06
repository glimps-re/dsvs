// Package plain streams the logs as they come, without an interface: the fallback when there is no
// interactive terminal, or when you just want to watch them scroll.
package plain

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"dsvs/internal/core"
)

type source struct {
	name    string
	journal *core.Journal
	ansi    int
	prefix  string
	seq     int
}

// Run copies everything that reaches the logs until the context is canceled.
func Run(ctx context.Context, session *core.Session, out io.Writer) {
	var sources []*source
	prefixes := map[string]string{}

	writer := bufio.NewWriter(out)
	flush := func() {
		sources = follow(session, sources)
		for _, src := range sources {
			prefixes[src.name] = src.prefix
		}
		series := make([][]core.Line, 0, len(sources))
		for _, src := range sources {
			lines, next, dropped := src.journal.Since(src.seq)
			src.seq = next
			if dropped > 0 {
				fmt.Fprintf(writer, "%s [%d lines lost]\n", src.prefix, dropped)
			}
			series = append(series, lines)
		}
		for _, line := range core.Merge(series...) {
			fmt.Fprintf(writer, "%s %s\n", prefixes[line.Src], line.Text)
		}
		writer.Flush()
	}

	events, unwatch := session.Watch()
	defer unwatch()
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case <-events:
			flush()
		}
	}
}

// follow adds the sources a reload brought in, and realigns the prefixes on the longest name. A
// source never leaves: its log will say nothing more, but what it already said stays in the stream.
func follow(session *core.Session, known []*source) []*source {
	fresh := []*source{{name: session.Log.Name(), journal: session.Log, ansi: 4}}
	for _, in := range session.Facades() {
		fresh = append(fresh, &source{name: in.Cfg.Name, journal: in.Log, ansi: 4})
	}
	for _, app := range session.Apps() {
		fresh = append(fresh, &source{name: app.Name, journal: app.Log, ansi: app.Cfg().Ansi})
	}

	out := known
	for _, src := range fresh {
		if !slices.ContainsFunc(out, func(other *source) bool { return other.name == src.name }) {
			out = append(out, src)
		}
	}
	width := 0
	for _, src := range out {
		width = max(width, utf8.RuneCountInString(src.name))
	}
	for _, src := range out {
		src.prefix = label(src.name, width, src.ansi)
	}
	return out
}

// label colors and pads a source name — in runes, not bytes: otherwise a name with accents would
// throw its column off.
func label(name string, width, ansi int) string {
	return fmt.Sprintf("\x1b[3%dm%s\x1b[0m", ansi, name+strings.Repeat(" ", max(0, width-utf8.RuneCountInString(name))))
}
