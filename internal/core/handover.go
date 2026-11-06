package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"dsvs/internal/config"
)

// HandoverVersion is the version of a handover's manifest. The binary that takes over refuses what
// it does not understand: keeping the old one is better than handing over into the void.
const HandoverVersion = 1

// Handover is what a session passes to its re-executed image: what nothing else says on the other
// side. What the kernel already holds — the processes, the terminals, the private network's
// addresses, the windows' size — only appears there as a descriptor number, or not at all, and the
// logs travel as text, as a terminal would have written them.
type Handover struct {
	Version int `json:"version"`
	// Listen and Lock are the session's attach socket and the lock that gives it its name. They
	// cross the exec: releasing them for the time of a handover would let a client knock on nothing
	// and launch a second session for the same configuration, which would take the name.
	Listen int `json:"listen"`
	Lock   int `json:"lock"`
	// IPs is the app → private network address table. The addresses themselves are already on lo:
	// that is in the kernel, and only this link is missing on the other side.
	IPs map[string]string `json:"ips,omitempty"`
	// Fallback is the reason the private network was done without, which the network card displays.
	Fallback string        `json:"fallback,omitempty"`
	Log      string        `json:"log"`
	Apps     []AppState    `json:"apps,omitempty"`
	Facades  []FacadeState `json:"facades,omitempty"`
	// Params are the session's intents: the new image replays the configuration with them, and the
	// file does not say them. A default is never among them: it is resolved again on the other
	// side.
	Params map[string]string `json:"params,omitempty"`
	// Answer is the response that built the configuration in force. The new image starts again from
	// it without evaluating anything, then replays the file as after a save: a binary that refuses
	// the file therefore keeps the configuration in force and says so, instead of losing the
	// session. Missing from a handover coming from an older binary, the new image evaluates the
	// file to start.
	Answer json.RawMessage `json:"answer,omitempty"`
}

// Load returns what an image opens on: the configuration in force on the other side of the
// handover, as its response says it, without evaluating anything; failing that — no handover, or a
// handover from an older binary —, the one the file builds.
func (h *Handover) Load(path string, watch []string, in config.Input) (config.Form, *config.Config, error) {
	if h == nil || h.Answer == nil {
		return config.Load(path, watch, in)
	}
	return config.Answered(path, watch, in, h.Answer)
}

// AppState is what an app carries: its log, what its runs gave, and its current run if there is
// one.
type AppState struct {
	Name     string    `json:"name"`
	Restarts int       `json:"restarts,omitempty"`
	Last     *Outcome  `json:"last,omitempty"`
	Log      string    `json:"log"`
	Run      *RunState `json:"run,omitempty"`
}

// RunState is the process to take over: how to find its terminal, its address and its wait again,
// plus the declaration it was launched with — it is what makes the drift from the current
// configuration, and the file does not say it.
type RunState struct {
	PID  int        `json:"pid"`
	TTY  int        `json:"tty"`
	IP   string     `json:"ip"`
	Port int        `json:"port"`
	Cfg  config.App `json:"cfg"`
	// Ready says the app was answering: the proxy takes its address back without a 502 window, and
	// there is nothing to probe.
	Ready   bool      `json:"ready,omitempty"`
	Status  Status    `json:"status"`
	Since   time.Time `json:"since"`
	ReadyAt time.Time `json:"readyAt,omitzero"`
}

// FacadeState carries only a log: the facade itself is reread from the configuration.
type FacadeState struct {
	Name string `json:"name"`
	Log  string `json:"log"`
}

// SetRenewal sets the gesture that replaces the session's binary without letting go of its apps.
// The gesture belongs to the process — argv, systemd box, descriptors to carry across — and the
// session only carries it to whoever asks. Setting it also says that the apps do not need
// Pdeathsig: the box's cgroup is the guarantee that replaces it, and it has to replace it,
// Pdeathsig firing precisely at the exec. To be set before Run.
func (s *Session) SetRenewal(how func() error) { s.renewal = how }

func (s *Session) handsOver() bool { return s.renewal != nil }

// Renew replaces the session's binary with the one on disk without letting go of its apps: it execs
// itself, its apps stay its children, and the attached interfaces reattach. A successful exec never
// returns; whatever comes back from here is the reason it could not, also told in the session's log
// — that is where one looks for it.
func (s *Session) Renew() error {
	if s.renewal == nil {
		return s.refused(errors.New("taking the binary on disk needs the systemd box: the apps would not survive the exec"))
	}
	s.Log.Add("-- taking the binary on disk, apps and all")
	if err := s.renewal(); err != nil {
		return s.refused(err)
	}
	return nil
}

func (s *Session) refused(err error) error {
	s.LogSink(s.Log)("!! " + err.Error())
	return err
}

// Export tells everything a handover has to carry. It dismantles nothing: the session keeps serving
// if the exec fails, and the descriptors it names are still its own — it is up to the caller to
// clear their FD_CLOEXEC, right before leaving.
func (s *Session) Export() (Handover, error) {
	s.mu.RLock()
	intents := maps.Clone(s.intents)
	s.mu.RUnlock()
	out := Handover{Version: HandoverVersion, Fallback: s.net.Mode().Reason, Log: s.Log.Dump(), Params: intents,
		Answer: s.Config().Answer}
	for _, in := range s.Facades() {
		out.Facades = append(out.Facades, FacadeState{Name: in.Cfg.Name, Log: in.Log.Dump()})
	}
	for _, app := range s.Apps() {
		state, err := app.export()
		if err != nil {
			return Handover{}, err
		}
		out.Apps = append(out.Apps, state)
	}
	return out, nil
}

// Adopt takes back a handover's state: the apps that were running become this session's again, with
// their process, their terminal and their address, and the logs are replayed through the path that
// normally writes them. The session, opened on the configuration in force on the other side
// (Handover.Load), then replays the file as after a save: it is this binary that says whether it
// applies it, and a refusal keeps the configuration in force. To be called before Run, which then
// puts each supervisor to work in the run it just found again. Failing here is fatal to the
// session: live apps nobody supervises anymore cannot be caught again, and it is better for the
// cgroup to take them down.
func (s *Session) Adopt(taken Handover) error {
	if taken.Version != HandoverVersion {
		return fmt.Errorf("handover format %d, not %d", taken.Version, HandoverVersion)
	}
	s.Log.Write([]byte(taken.Log))
	s.mu.Lock()
	maps.Copy(s.intents, taken.Params)
	s.mu.Unlock()
	for _, facade := range taken.Facades {
		if in := s.facade(facade.Name); in != nil {
			in.Log.Write([]byte(facade.Log))
		}
	}
	for _, state := range taken.Apps {
		app := s.App(state.Name)
		if app == nil {
			// The configuration no longer declares it: it takes back its place behind the others,
			// for the time of its process, exactly as a configuration reload leaves it there.
			app = s.keep(state.Name)
		}
		if err := app.adopt(state); err != nil {
			return err
		}
	}
	_ = s.replay() // a refusal is told in the log and in the status, and the session serves anyway
	return nil
}

// keep returns the supervisor of an app the configuration no longer declares: the handover found it
// running, and it keeps its place as long as its process lives.
func (s *Session) keep(name string) *App {
	s.mu.Lock()
	defer s.mu.Unlock()
	app := s.recruit(name)
	s.apps = append(s.apps, app)
	return app
}

func (s *Session) facade(name string) *Ingress {
	for _, in := range s.Facades() {
		if in.Cfg.Name == name {
			return in
		}
	}
	return nil
}

func (s *App) export() (AppState, error) {
	out := AppState{Name: s.Name, Log: s.Log.Dump()}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out.Restarts, out.Last = s.snap.Restarts, s.snap.Last
	if s.cur == nil {
		return out, nil
	}
	tty, err := fdOf(s.cur.tty)
	if err != nil {
		return AppState{}, fmt.Errorf("%s: its terminal: %w", s.Name, err)
	}
	out.Run = &RunState{
		PID: s.cur.proc.Pid, TTY: tty, IP: s.cur.ep.IP, Port: s.cur.ep.Port, Cfg: s.run,
		Ready: s.live.Load() != nil, Status: s.snap.Status, Since: s.snap.Since, ReadyAt: s.snap.ReadyAt,
	}
	return out, nil
}

func (s *App) adopt(state AppState) error {
	s.Log.Write([]byte(state.Log))
	if state.Run == nil {
		s.publish(nil, Snapshot{Status: Idle, Last: state.Last, Restarts: state.Restarts})
		return nil
	}
	// A missing descriptor is a terminal that did not cross over: the app runs, and nobody reads it
	// or can write to it anymore.
	if _, err := unix.FcntlInt(uintptr(state.Run.TTY), unix.F_GETFD, 0); err != nil {
		return fmt.Errorf("%s: no terminal on fd %d: %w", s.Name, state.Run.TTY, err)
	}
	proc, err := os.FindProcess(state.Run.PID)
	if err != nil {
		return fmt.Errorf("%s: pid %d: %w", s.Name, state.Run.PID, err)
	}
	r := &run{
		proc: proc,
		tty:  os.NewFile(uintptr(state.Run.TTY), s.Name),
		ep:   Endpoint{IP: state.Run.IP, Port: state.Run.Port},
	}
	if state.Run.Ready {
		s.live.Store(&backend{name: s.Name, ep: r.ep, net: s.net})
	}
	s.mu.Lock()
	s.run = state.Run.Cfg.Resolved()
	s.mu.Unlock()
	s.publish(r, Snapshot{Status: state.Run.Status, PID: state.Run.PID, Since: state.Run.Since,
		ReadyAt: state.Run.ReadyAt, Last: state.Last, Restarts: state.Restarts})
	return nil
}

// resume takes back the run an adoption set, when there is one, and says whether another must be
// started after. Without adoption there is nothing to take back, and the app waits for a command as
// usual.
func (s *App) resume(ctx context.Context) bool {
	s.mu.RLock()
	r := s.cur
	s.mu.RUnlock()
	if r == nil {
		return false
	}
	return s.follow(ctx, r, s.Snapshot())
}

// fdOf returns a file's descriptor number without touching it: os.File.Fd would switch it back to
// blocking and take it out of the poller, which cuts off the ongoing terminal read.
func fdOf(file *os.File) (int, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	fd := -1
	if err := raw.Control(func(handle uintptr) { fd = int(handle) }); err != nil {
		return 0, err
	}
	return fd, nil
}
