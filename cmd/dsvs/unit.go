package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"dsvs/internal/hub"
	"dsvs/internal/ui/tray"
)

// unitFile is the unit that starts the service at session login. graphical-session.target would be
// of no use: measured under Qubes/xfce, it is never activated, whereas default.target is. But
// default.target is reached before xfce imports DISPLAY into the manager's environment — measured,
// three seconds earlier — and a --tray without a display dies at once on "empty display string": the
// icon was thus missing at every login.
//
// Hence Restart=on-failure, the only thing to add: each attempt reads the manager's environment
// again, and the service holds as soon as DISPLAY is there. Twenty attempts five seconds apart cover
// a hundred seconds or so of session startup, which the three measured ones fit in with room to
// spare; past that, a machine without a display gives up instead of looping forever, and `dsvs`
// restarts the service when needed. The window is wider than the burst so that the count decides, not
// its timing. An icon already in place makes the service exit with zero — which on-failure does not
// restart.
const unitFile = `[Unit]
Description=%s — dev-server control panel
StartLimitIntervalSec=180
StartLimitBurst=20

[Service]
ExecStart=%s --tray
%sRestart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

// unitFor is the unit of this very binary, with the instance's settings in its [Service] section.
// Two systemd writing rules apply, and they live here alone: it splits ExecStart at spaces, hence the
// quotes around the path, and it reads a % as a specifier, hence the doubled % — an install directory
// may contain both. The setting lines are already unit text, they are not escaped again.
func unitFor(exe string, environment []string) string {
	systemd := strings.NewReplacer("%", "%%")
	settings := ""
	for _, line := range environment {
		settings += line + "\n"
	}
	return fmt.Sprintf(unitFile, systemd.Replace(filepath.Base(exe)), systemd.Replace(strconv.Quote(exe)), settings)
}

// installUnit writes the service's systemd --user unit, enables it for the next logins, and leaves the
// service running behind it. The unit's name comes from the binary's, like its sockets: two binaries
// fight over neither.
func installUnit(shift int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	file, err := unitFileOf(exe)
	if err != nil {
		return err
	}
	dir, unit := filepath.Split(file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	settings := environment(file, shift)
	if err := os.WriteFile(file, []byte(unitFor(exe, settings)), 0o644); err != nil {
		return err
	}
	carrying := shiftIn(settings)
	installed := unit + " installed" + carrying
	// A running service is someone behind the icon's socket, and only one can hold it there: the question
	// has a single reader, whatever started it — the unit, `dsvs`, or a hand. So `--now` is only asked
	// when there is an icon to show and nobody to hold it: without a status bar the unit would die on the
	// spot, and with an icon already there it would leave without a word.
	running := hub.Answers(hub.TrayAddr())
	enable := []string{"--user", "enable", unit}
	if !running && tray.Available() {
		enable = append(enable, "--now")
	}
	// Writing the unit is the contract, enabling it is good will: outside a systemd session — a
	// container, an ssh session, an install script — there is nobody to ask, and failing the whole
	// installation for that would lie about what was just written.
	if out, err := exec.Command("systemctl", enable...).CombinedOutput(); err != nil {
		fmt.Printf("%s written in %s%s\n", unit, dir, carrying)
		fmt.Fprintf(os.Stderr, "note: it is not enabled yet (%v: %s); from a systemd session, run: systemctl --user enable --now %s\n",
			err, firstLine(string(out)), unit)
		return nil
	}
	// A started service is not promised, it is observed: the same socket answers, or nothing does — and
	// nothing means the status bar is missing here, the unit staying for the next session.
	switch {
	case running:
		fmt.Printf("%s; service already running\n", installed)
	case hub.Awaits(hub.TrayAddr()):
		fmt.Printf("%s; service started, its icon is in the status bar\n", installed)
	default:
		fmt.Printf("%s; the service starts at session start\n", installed)
	}
	return nil
}

// unitFileOf is where the unit of this binary lives. Its name comes from the binary's, like its
// sockets: two binaries fight over neither.
func unitFileOf(exe string) (string, error) {
	config, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "systemd", "user", filepath.Base(exe)+".service"), nil
}

// environment returns the Environment= lines of the [Service] section. A requested shift replaces
// them; without a request, those of the unit already in place remain — the unit remembers its
// instance's settings, and an update, which rewrites the unit without asking anything, does not lose
// them. No unit in place, nothing to keep.
func environment(unit string, shift int) []string {
	if shift != 0 {
		return []string{fmt.Sprintf("Environment=%s=%d", shiftEnv, shift)}
	}
	written, _ := os.ReadFile(unit)
	var kept []string
	for _, line := range strings.Split(string(written), "\n") {
		if strings.HasPrefix(line, "Environment=") {
			kept = append(kept, line)
		}
	}
	return kept
}

// shiftIn tells the shift these settings carry, as the message announces it: the flag's as well as
// the one just kept, since the same unit carries them.
func shiftIn(environment []string) string {
	for _, line := range environment {
		if value, found := strings.CutPrefix(line, "Environment="+shiftEnv+"="); found {
			return " with port shift " + value
		}
	}
	return ""
}

func configDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config"), nil
}

// firstLine keeps, from a chatty command, the line that says why: it is the first, and systemctl
// writes several.
func firstLine(said string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(said), "\n")
	return line
}
