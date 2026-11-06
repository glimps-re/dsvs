package attach

import (
	"encoding/json"
	"time"

	"dsvs/internal/build"
	"dsvs/internal/config"
)

// State is what a session tells about itself to whoever drives it without watching it: enough to fill
// a panel. No logs — those are the TUI's job, and that is what keeps this dialogue small.
type State struct {
	Config string `json:"config"`
	Port   int    `json:"port"`
	// Version is that of the binary holding the session: it is what tells, after an update, whether it
	// took the new one or kept its own.
	Version    string      `json:"version,omitempty"`
	Apps       []App       `json:"apps"`
	Facades    []Facade    `json:"facades"`
	Interfaces []Interface `json:"interfaces"`
	// Params are the fields of the last evaluated form, hidden ones included: an interface does not show
	// those, a tool reads them.
	Params []config.Field `json:"params"`
	// Errors are the errors of that form that no field carries, Notices what it says without refusing
	// anything.
	Errors  []string `json:"errors"`
	Notices []string `json:"notices"`
	// Observed is what the session sees of the machine; absent when nothing observes it.
	Observed *config.Observed `json:"observed,omitempty"`
	// NotApplied says, in one line, why the configuration in force is not the one the file builds: the
	// last evaluation failed, or its form is invalid, and the targets are the previous ones. Absent when
	// it is.
	NotApplied string `json:"notApplied,omitempty"`
}

// MarshalJSON writes every list, empty ones included: a tool reads "none" in [], without having to
// guess what a missing key means.
func (s State) MarshalJSON() ([]byte, error) {
	type plain State
	s.Apps, s.Facades, s.Interfaces = append([]App{}, s.Apps...), append([]Facade{}, s.Facades...), append([]Interface{}, s.Interfaces...)
	s.Params, s.Errors, s.Notices = append([]config.Field{}, s.Params...), append([]string{}, s.Errors...), append([]string{}, s.Notices...)
	return json.Marshal(plain(s))
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
	// Fell says whether that last run fell, according to core: Last's text tells the story, it is not
	// parsed back.
	Fell bool `json:"fell,omitempty"`
	// Astray says where an app that is starting listens when it does not listen at its address.
	Astray string `json:"astray,omitempty"`
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
	cfg, form := s.session.Config(), s.session.Form()
	out := State{Config: cfg.Path, Port: cfg.Port, Version: build.Version, Interfaces: s.attached(),
		Params: form.Fields, Errors: form.Errors, Notices: form.Notices, Observed: s.session.Observed(), NotApplied: s.session.NotApplied()}
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
			one.Last, one.Fell = snap.Last.String(), snap.Last.Fell()
		}
		if snap.Astray != nil {
			one.Astray = snap.Astray.String()
		}
		out.Apps = append(out.Apps, one)
	}
	for _, in := range s.session.Facades() {
		out.Facades = append(out.Facades, Facade{Host: in.Host()})
	}
	return out
}
