package attach

import (
	"time"

	"dsvs/internal/build"
)

// State is what a session tells about itself to whoever drives it without watching it: enough to fill
// a panel. No logs — those are the TUI's job, and that is what keeps this dialogue small.
type State struct {
	Config string `json:"config"`
	Port   int    `json:"port"`
	// Version is that of the binary holding the session: it is what tells, after an update, whether it
	// took the new one or kept its own.
	Version    string      `json:"version,omitempty"`
	Apps       []App       `json:"apps,omitempty"`
	Facades    []Facade    `json:"facades,omitempty"`
	Interfaces []Interface `json:"interfaces,omitempty"`
}

// App is the state of an app as a panel shows it: what it is doing, since when, and how its last run
// went.
type App struct {
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Ready  bool      `json:"ready,omitempty"`
	Drift  string    `json:"drift,omitempty"`
	PID    int       `json:"pid,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	Last   string    `json:"last,omitempty"`
}

// Facade is a facade and the host it is opened by.
type Facade struct {
	Host string `json:"host"`
}

// Interface is an interface attached to the session. The window is what it takes to bring it back on
// screen when looking for it.
type Interface struct {
	Kind   string `json:"kind"`
	Window string `json:"window,omitempty"`
}

// Log is a log line as a session renders it to whoever reads it without attaching: the tail of the
// logs, once, where an interface consumes the stream.
type Log struct {
	At   time.Time `json:"at"`
	Src  string    `json:"src"`
	Text string    `json:"text"`
}

// state reads the session at this instant. Nothing is kept from one call to the next: it is a
// snapshot, and the driver receives a fresh one on every change.
func (s *Server) state() State {
	cfg := s.session.Config()
	out := State{Config: cfg.Path, Port: cfg.Port, Version: build.Version, Interfaces: s.attached()}
	for _, app := range s.session.Apps() {
		snap := app.Snapshot()
		one := App{
			Name:   app.Name,
			Status: snap.Status.String(),
			Ready:  app.IsReady(),
			Drift:  app.Drift().String(),
			PID:    snap.PID,
			Since:  snap.Since,
		}
		if snap.Last != nil {
			one.Last = snap.Last.String()
		}
		out.Apps = append(out.Apps, one)
	}
	for _, in := range s.session.Facades() {
		out.Facades = append(out.Facades, Facade{Host: in.Host()})
	}
	return out
}
