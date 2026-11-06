package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"dsvs/internal/attach"
)

// settling is the time given to the session to say what the order changed. An order that changes
// nothing — stopping what is already stopped — pushes no state, and there is no waiting for a state
// that will not come. It is not a wait for "ready": state is there for that.
const settling = 300 * time.Millisecond

// stopping bounds the wait for a session that is going away. It is the TimeoutStopSec of its systemd
// box: beyond it, the session no longer decides.
const stopping = 30 * time.Second

// bare is dsvs with no command: the session of the configuration here is there, or it is born, and
// its state is written. Nothing is ordered; --tui opens the interface on it
// instead of returning.
func bare(s *shape) error {
	if s.tui {
		return lend(s, nil)
	}
	link, err := ensure(s)
	if err != nil {
		return err
	}
	defer link.Close()
	return writeState(os.Stdout, <-link.States())
}

// order carries an order to the session here and writes what it says about it. start ensures it — it
// is born if need be; the others require it alive, since there is nothing to stop
// or reload in a session that is not there. An empty list of names means "every app": the session
// resolves it, and it is what --all sends.
func order(s *shape, verb string, names []string) error {
	if s.tui && verb == "start" {
		return lend(s, names)
	}
	// start ensures the session, the others require it alive.
	open := watching
	if verb == "start" {
		open = ensure
	}
	link, err := open(s)
	if err != nil {
		return err
	}
	defer link.Close()
	last := <-link.States()
	if err := link.Order(verb, names...); err != nil {
		return err
	}
	return writeState(os.Stdout, settled(link, last))
}

// reloading makes the session here read its configuration again. The order is about the whole
// session: a name would mean nothing there, and refusing it is better than ignoring it.
func reloading(s *shape) func([]string) error {
	return func(names []string) error {
		if len(names) > 0 {
			return errors.New("reload takes no name: it is the whole session")
		}
		return order(s, "reload", nil)
	}
}

// closing closes the session here: its apps stop, its port is released, and its link closing is the
// only proof that it is gone.
func closing(s *shape) func([]string) error {
	return func(names []string) error {
		if len(names) > 0 {
			return errors.New("quit closes the whole session; it takes no name")
		}
		link, err := watching(s)
		if err != nil {
			return err
		}
		defer link.Close()
		if err := link.Order("quit"); err != nil {
			return err
		}
		waited := time.After(stopping)
		for {
			select {
			case _, live := <-link.States():
				if !live {
					_, err := fmt.Println("session closed")
					return err
				}
			case <-waited:
				_, err := fmt.Println("the session is still stopping its apps")
				return err
			}
		}
	}
}

// showState writes the state of the session here, as it pushes it to whoever drives it.
func showState(s *shape, asJSON bool) error {
	link, err := watching(s)
	if err != nil {
		return err
	}
	defer link.Close()
	state := <-link.States()
	if asJSON {
		return write(os.Stdout, state)
	}
	return writeState(os.Stdout, state)
}

// showLogs writes the tail of the logs, or follows them. Following is the attachment that shows only
// the stream: the session then writes on this terminal itself, and there is nothing to filter here.
func showLogs(s *shape, n int, follow, asJSON bool, names []string) error {
	config, err := resolve(s.config)
	if err != nil {
		return err
	}
	if follow {
		return attach.Follow(config)
	}
	logs, err := attach.Tail(config, n, names)
	if err != nil {
		return err
	}
	if asJSON {
		return write(os.Stdout, logs)
	}
	return writeLogs(os.Stdout, logs)
}

// writeState writes a state for a human as well as for an agent: the session, then its apps in
// columns, then what surrounds it. It is a rendering and nothing else. An empty column says there is
// nothing to say, where a zero would read as a measurement.
func writeState(out io.Writer, state attach.State) error {
	w := bufio.NewWriter(out)
	fmt.Fprintf(w, "config   %s\nport     %d\n", state.Config, state.Port)
	if state.Version != "" {
		fmt.Fprintf(w, "version  %s\n", state.Version)
	}
	if len(state.Apps) == 0 {
		fmt.Fprintln(w, "\nno app declared")
	} else {
		fmt.Fprintln(w)
		var block bytes.Buffer
		cols := tabwriter.NewWriter(&block, 0, 0, 2, ' ', 0)
		fmt.Fprintln(cols, "app\tstatus\tready\tpid\tsince\tlast")
		for _, app := range state.Apps {
			ready, pid, since := "", "", ""
			if app.Ready {
				ready = "yes"
			}
			if app.PID > 0 {
				pid = strconv.Itoa(app.PID)
			}
			if !app.Since.IsZero() {
				since = app.Since.Format("15:04:05")
			}
			fmt.Fprintf(cols, "%s\t%s\t%s\t%s\t%s\t%s\n",
				app.Name, doing(app.Status, app.Drift), ready, pid, since, app.Last)
		}
		cols.Flush()
		// Alignment leaves blanks after an empty column: a stopped app has neither pid nor time, and a
		// line does not end on blanks.
		for _, line := range strings.Split(strings.TrimRight(block.String(), "\n"), "\n") {
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
	}
	facades := make([]string, 0, len(state.Facades))
	for _, facade := range state.Facades {
		facades = append(facades, fmt.Sprintf("%s:%d", facade.Host, state.Port))
	}
	attached := make([]string, 0, len(state.Interfaces))
	for _, seen := range state.Interfaces {
		attached = append(attached, seen.Kind)
	}
	fmt.Fprintf(w, "\nfacades     %s\ninterfaces  %s\n", listed(facades), listed(attached))
	return w.Flush()
}

// writeLogs writes log lines as the stream writes them: the source, aligned, then the line as the app
// wrote it, colours included. Alignment is counted in runes, otherwise an accented name would keep
// its column askew.
func writeLogs(out io.Writer, logs []attach.Log) error {
	width := 0
	for _, line := range logs {
		width = max(width, utf8.RuneCountInString(line.Src))
	}
	w := bufio.NewWriter(out)
	for _, line := range logs {
		padding := strings.Repeat(" ", width-utf8.RuneCountInString(line.Src))
		fmt.Fprintf(w, "%s%s  %s\n", line.Src, padding, line.Text)
	}
	return w.Flush()
}

// write writes an answer as indented JSON: that is what a script or an agent reads.
func write(out io.Writer, answer any) error {
	text, err := json.MarshalIndent(answer, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", text)
	return err
}

// watching opens the link with the session here, required to be alive: a session is not started just
// to ask it to stop, and its absence is reported.
func watching(s *shape) (*attach.Link, error) {
	config, err := resolve(s.config)
	if err != nil {
		return nil, err
	}
	return attach.Watch(config)
}

// ensure returns the link with the session here, starting it if it is not there.
func ensure(s *shape) (*attach.Link, error) {
	config, err := resolve(s.config)
	if err != nil {
		return nil, err
	}
	return attach.Ensure(config, func() error { return spawn(config, s.args()) })
}

// settled returns the state the session pushes after an order, or the last known one when the order
// changes nothing.
func settled(link *attach.Link, last attach.State) attach.State {
	select {
	case next, live := <-link.States():
		if live {
			return next
		}
	case <-time.After(settling):
	}
	return last
}

// doing says what an app does, and the drift from its declaration when there is one: an app running
// with the previous one, or that nothing declares anymore.
func doing(what, drift string) string {
	if drift == "" {
		return what
	}
	return what + " (" + drift + ")"
}

// listed writes a list on one line, or says there is none: an empty line would read as a failed
// rendering.
func listed(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, "  ")
}
