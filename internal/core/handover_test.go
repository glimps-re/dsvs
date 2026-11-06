package core

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/creack/pty"

	"dsvs/internal/config"
)

// A handover's manifest carries what nothing else will say on the other side: each app's process,
// its terminal, its address, the declaration it was launched with, and the logs. It crosses over as
// JSON, and nothing is lost on the way.
func TestHandoverCarriesWhatNothingElseSays(t *testing.T) {
	t.Parallel()
	session, app, _ := start(t, sleeper)
	app.Start()
	waitFor(t, "ready", func() bool { return app.Snapshot().Status == Ready })

	exported, err := session.Export()
	if err != nil {
		t.Fatal(err)
	}
	text, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	var taken Handover
	if err := json.Unmarshal(text, &taken); err != nil {
		t.Fatal(err)
	}

	if taken.Version != HandoverVersion {
		t.Errorf("manifest version = %d, want %d", taken.Version, HandoverVersion)
	}
	if len(taken.Apps) != 1 || taken.Apps[0].Name != "app" {
		t.Fatalf("manifest apps = %+v", taken.Apps)
	}
	carried, snap := taken.Apps[0], app.Snapshot()
	if carried.Run == nil {
		t.Fatal("a running app crosses over without its run")
	}
	if carried.Run.PID != snap.PID {
		t.Errorf("pid = %d, want %d", carried.Run.PID, snap.PID)
	}
	if !carried.Run.Ready || carried.Run.Status != Ready {
		t.Errorf("run = %+v, want ready", carried.Run)
	}
	if got := carried.Run.Cfg.Command; strings.Join(got, " ") != strings.Join(sleeper, " ") {
		t.Errorf("run declaration = %v, want %v", got, sleeper)
	}
	if carried.Run.IP == "" || carried.Run.Port == 0 {
		t.Errorf("run address = %s:%d, want it set", carried.Run.IP, carried.Run.Port)
	}
	// The terminal only travels by its number: it is indeed the one the session holds.
	if fd, err := fdOf(app.cur.tty); err != nil || carried.Run.TTY != fd {
		t.Errorf("terminal descriptor = %d, want %d (%v)", carried.Run.TTY, fd, err)
	}
	if !strings.Contains(carried.Log, strings.Join(sleeper, " ")) {
		t.Errorf("the app's log does not cross over: %q", carried.Log)
	}
	if len(taken.Facades) != 1 || taken.Facades[0].Name != session.Facades()[0].Cfg.Name {
		t.Errorf("manifest facades = %+v", taken.Facades)
	}
	if !strings.Contains(taken.Log, "listening on port") {
		t.Errorf("the session's log does not cross over: %q", taken.Log)
	}
}

// An adopted app keeps its process: the session takes back its wait, reads its terminal, writes to
// it, and it is the app's own stop that ends the run.
func TestAdoptedRunKeepsItsProcess(t *testing.T) {
	t.Parallel()
	cfg, spy := fixture(t, sleeper)
	session := NewSession(config.Form{}, cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy })

	tty, pid := onAPty(t, "sh", "-c", "read line; echo adopted; sleep 300 & wait")
	if err := session.Adopt(handoverOf(t, "app", cfg.Apps[0], tty, pid)); err != nil {
		t.Fatal(err)
	}
	supervising(t, session)

	app := session.App("app")
	if snap := app.Snapshot(); snap.PID != pid || snap.Status != Ready {
		t.Fatalf("snapshot = %+v, want ready on pid %d", snap, pid)
	}
	if !app.IsReady() {
		t.Error("an app that answered before the handover must answer after")
	}
	// The run's declaration crossed the JSON: it must stay equal to the file's, otherwise every app
	// would believe it needs a restart after each handover.
	if got := app.Drift(); got != Settled {
		t.Errorf("drift = %v, want %v: the declaration did not change", got, Settled)
	}

	// The inherited terminal can be read and written: the app speaks, the session hears it.
	if err := app.Send("go\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the app heard after the adoption", func() bool {
		return strings.Contains(logged(app.Log), "adopted")
	})

	// The resumed wait ends the run: without it, the app would stay ready forever.
	app.Stop()
	waitFor(t, "the app stopped", func() bool { return app.Snapshot().Status == Idle })
	if last := app.Snapshot().Last; last == nil || last.Ending != Stopped {
		t.Errorf("end of run = %+v, want a requested stop", last)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("process %d still runs", pid)
	}
}

// An app the configuration no longer declares but that was running at the handover takes back its
// place: the session finds it with its run's declaration, as a configuration reload leaves it
// there.
func TestAdoptedAppTheConfigurationDroppedKeepsItsPlace(t *testing.T) {
	t.Parallel()
	cfg, spy := fixture(t, sleeper)
	dropped := cfg.Apps[0]
	cfg.Apps = nil
	session := NewSession(config.Form{}, cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy })

	tty, pid := onAPty(t, sleeper...)
	if err := session.Adopt(handoverOf(t, "app", dropped, tty, pid)); err != nil {
		t.Fatal(err)
	}
	supervising(t, session)

	app := session.App("app")
	if app == nil {
		t.Fatal("the adopted app has no place in the session")
	}
	if got := app.Drift(); got != Dropped {
		t.Errorf("drift = %v, want %v", got, Dropped)
	}
	if got := app.Cfg().Command; strings.Join(got, " ") != strings.Join(dropped.Command, " ") {
		t.Errorf("declaration = %v, want the adopted run's", got)
	}
}

// A manifest of an unknown version is not adopted: better keep the running binary than hand over
// into the void.
func TestAnUnknownHandoverIsRefused(t *testing.T) {
	t.Parallel()
	cfg, spy := fixture(t, sleeper)
	session := NewSession(config.Form{}, cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy })
	if err := session.Adopt(Handover{Version: HandoverVersion + 1}); err == nil {
		t.Error("a manifest of another version was accepted")
	}
}

// Without a handover gesture set, the verb is refused with its reason: outside a systemd box, an
// exec would kill the apps.
func TestRenewIsRefusedWithoutTheBox(t *testing.T) {
	t.Parallel()
	cfg, spy := fixture(t, sleeper)
	session := NewSession(config.Form{}, cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return spy })
	err := session.Renew()
	if err == nil {
		t.Fatal("the handover was accepted without a gesture set")
	}
	if !strings.Contains(logged(session.Log), err.Error()) {
		t.Errorf("the refusal cannot be read in the session's log: %q", logged(session.Log))
	}
}

// onAPty launches a process on its own terminal, exactly like an app run, and returns the master
// and its pid: that is what a handover carries across.
func onAPty(t *testing.T, command ...string) (*os.File, int) {
	t.Helper()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = t.TempDir()
	tty, err := pty.StartWithAttrs(cmd, &pty.Winsize{Cols: 80, Rows: 24},
		&syscall.SysProcAttr{Setsid: true, Setctty: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		tty.Close()
	})
	return tty, cmd.Process.Pid
}

// handoverOf writes the manifest of a session in which this app runs and answers.
func handoverOf(t *testing.T, name string, declared config.App, tty *os.File, pid int) Handover {
	t.Helper()
	fd, err := fdOf(tty)
	if err != nil {
		t.Fatal(err)
	}
	// A descriptor has only one owner. On the other side of a real handover the exec has wiped the
	// image that held this one, and the adoption alone inherits it; here the launcher and the
	// adoption live in the same process, so a copy crosses over. Without it both would close the
	// same number, and the second close would take down what a neighboring test opened in the
	// meantime.
	carried, err := syscall.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	// The manifest goes through JSON as in a real handover: what the format does not carry must be
	// found on the other side, or the adoption will lie.
	written, err := json.Marshal(Handover{
		Version: HandoverVersion,
		Apps: []AppState{{Name: name, Run: &RunState{
			PID: pid, TTY: carried, IP: "127.0.0.1", Port: 4321, Cfg: declared,
			Ready: true, Status: Ready,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var taken Handover
	if err := json.Unmarshal(written, &taken); err != nil {
		t.Fatal(err)
	}
	return taken
}

func logged(log *Journal) string {
	var lines []string
	for _, line := range log.Lines() {
		lines = append(lines, line.Text)
	}
	return strings.Join(lines, "\n")
}

// The session's intents cross the handover, and only they do: the file does not say them, and a
// default is resolved again on the other side. A manifest from before parameters carries none, and
// is adopted anyway, on the defaults.
func TestTheHandoverCarriesTheIntents(t *testing.T) {
	t.Parallel()
	session, _ := paramsSession(t)
	if exported, err := session.Export(); err != nil || len(exported.Params) != 0 {
		t.Errorf("without a choice, the manifest carries %v (%v): a default is not an intent", exported.Params, err)
	}
	if err := session.Set(map[string]string{"env": "staging"}, "cli"); err != nil {
		t.Fatal(err)
	}
	exported, err := session.Export()
	if err != nil {
		t.Fatal(err)
	}
	text, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), `"params":{"env":"staging"}`) {
		t.Errorf("the manifest does not carry the intents: %s", text)
	}

	heir, _ := paramsSession(t)
	if err := heir.Adopt(Handover{Version: HandoverVersion, Params: exported.Params}); err != nil {
		t.Fatal(err)
	}
	if got := apiTarget(t, heir); got != "https://staging-0.example" {
		t.Errorf("the heir leads /api to %q, want the staging chosen before the handover", got)
	}

	var old Handover
	if err := json.Unmarshal([]byte(`{"version":1,"listen":3,"lock":4,"log":""}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Params != nil {
		t.Errorf("params = %v, want nil for a manifest without", old.Params)
	}
	if err := session.Adopt(old); err != nil {
		t.Errorf("Adopt of a v1 manifest without params: %v", err)
	}
}

// A handover starts again from the configuration in force without evaluating anything, then replays
// the file: the one the new binary refuses does not make it lose the session, which serves on the
// previous configuration and says why. Without a response — a handover from an older binary —, the
// new image evaluates.
func TestAHandoverKeepsTheConfigurationTheNewBinaryRefuses(t *testing.T) {
	t.Parallel()
	session, rewrite := paramsSession(t)
	if err := session.Set(map[string]string{"env": "staging"}, "cli"); err != nil {
		t.Fatal(err)
	}
	exported, err := session.Export()
	if err != nil {
		t.Fatal(err)
	}
	text, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	var taken Handover
	if err := json.Unmarshal(text, &taken); err != nil {
		t.Fatal(err)
	}
	path := session.Config().Path
	rewrite(`export const params = {};
export default { port: 4300, ingress: [], apps: [] };`)
	if _, _, err := (*Handover)(nil).Load(path, nil, config.Input{}); err == nil {
		t.Fatal("without a handover, the refused file loads")
	}

	form, cfg, err := taken.Load(path, nil, InputFor(path, nil, taken.Params, nil))
	if err != nil {
		t.Fatalf("the new image does not start again from the configuration in force: %v", err)
	}
	heir := NewSession(form, *cfg, heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	if err := heir.Adopt(taken); err != nil {
		t.Fatal(err)
	}
	supervising(t, heir)
	if got := apiTarget(t, heir); got != "https://staging-0.example" {
		t.Errorf("the heir leads /api to %q, want the configuration in force before the handover", got)
	}
	if got := heir.NotApplied(); !strings.Contains(got, "export const params is replaced by form()") {
		t.Errorf("not applied = %q, want the new binary's refusal", got)
	}
}

// A departed app's log crosses a handover: an update is not the end of the session.
func TestTheJournalOfAnAppGoneCrossesTheHandover(t *testing.T) {
	t.Parallel()
	session, declare := declaring(t, "front", "docs")
	session.App("docs").Log.Add("docs spoke")
	declare("front")
	if err := session.Reload(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "docs partie", func() bool { return session.App("docs") == nil })

	taken, err := session.Export()
	if err != nil {
		t.Fatal(err)
	}
	next := NewSession(session.Form(), session.Config(), heldNet(t), ownPort{}, func(*Session) Gateway { return &spyGateway{} })
	if err := next.Adopt(taken); err != nil {
		t.Fatal(err)
	}
	journals, err := next.Journals([]string{"docs"})
	if err != nil || len(journals) != 1 || !strings.Contains(journals[0].Dump(), "docs spoke") {
		t.Errorf("docs log after the handover = %v, %v", journals, err)
	}
}
