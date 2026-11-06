package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Writing the unit is the contract: without a systemd session to ask for enabling, the file is
// written all the same and the installation calling it does not fail. The name comes from the binary.
func TestInstallUnitWritesTheUnitWithoutASystemdSession(t *testing.T) {
	config := hidden(t)

	if err := installUnit(0); err != nil {
		t.Fatalf("installUnit = %v, want the unit written all the same", err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	unit := written(t, config)
	if got := started(t, unit); got != exe {
		t.Errorf("unit:\n%s\nsystemd would start %q, want %q", unit, got, exe)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("unit:\n%s\nwant WantedBy=default.target", unit)
	}
	// The display arrives after default.target: without Restart, the first attempt dies without DISPLAY
	// and the icon is missing for the whole session.
	if !strings.Contains(unit, "Restart=on-failure") {
		t.Errorf("unit:\n%s\nwant Restart=on-failure", unit)
	}
}

// The requirement is not an encoding but a path systemd reads back whole: an install directory may
// contain a space, which would split ExecStart, and a % followed by a letter, which it would replace
// with the specifier's value.
func TestTheUnitCarriesAPathSystemdReadsBackWhole(t *testing.T) {
	exe := "/home/someone/my tools/100%h new/dsvs-daemon"
	if got := started(t, unitFor(exe, nil)); got != exe {
		t.Errorf("systemd would start %q; want %q", got, exe)
	}
}

// The port shift is a setting of the instance: it lives in the unit, which puts it in the service's
// environment, from which everything it starts inherits it. And the unit remembers it: reinstalling
// it without asking anything — which every update does — must not make it lose it.
func TestTheUnitRemembersThePortShift(t *testing.T) {
	config := hidden(t)

	if err := installUnit(100); err != nil {
		t.Fatalf("installUnit(100) = %v", err)
	}
	unit := written(t, config)
	if !strings.Contains(unit, "Environment=DSVS_PORT_SHIFT=100") {
		t.Errorf("unit:\n%s\nwant Environment=DSVS_PORT_SHIFT=100", unit)
	}

	if err := installUnit(0); err != nil {
		t.Fatalf("installUnit(0) = %v", err)
	}
	if kept := written(t, config); kept != unit {
		t.Errorf("unit rewritten without a requested shift:\n%s\nwant the previous one:\n%s", kept, unit)
	}
}

// With no shift anywhere, the unit has no setting to carry: the [Service] section stays the one it
// always had.
func TestTheUnitCarriesNoSettingWithoutAShift(t *testing.T) {
	config := hidden(t)

	if err := installUnit(0); err != nil {
		t.Fatalf("installUnit(0) = %v", err)
	}
	if unit := written(t, config); strings.Contains(unit, "Environment=") {
		t.Errorf("unit:\n%s\nwant no Environment=", unit)
	}
}

// hidden gives the test its own configuration directory, out of systemd's reach: otherwise it would
// enable a unit in the session of whoever runs it.
func hidden(t *testing.T) string {
	t.Helper()
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "none"))
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent")
	return config
}

// written returns this binary's unit as it was written in this configuration directory.
func written(t *testing.T, config string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(config, "systemd", "user", filepath.Base(exe)+".service"))
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	return string(unit)
}

// started returns the binary systemd would start for this unit, reading ExecStart back as it does:
// specifiers replaced, then the command, which is the first word, quotes included.
func started(t *testing.T, unit string) string {
	t.Helper()
	command := ""
	for _, line := range strings.Split(unit, "\n") {
		if rest, found := strings.CutPrefix(line, "ExecStart="); found {
			command = expanded(rest)
		}
	}
	if command == "" {
		t.Fatalf("unit:\n%s\nwithout ExecStart", unit)
	}
	if quoted, found := strings.CutPrefix(command, `"`); found {
		word, _, _ := strings.Cut(quoted, `"`)
		return word
	}
	word, _, _ := strings.Cut(command, " ")
	return word
}

// expanded replaces specifiers as systemd does: %% gives a %, and any other % followed by a letter
// gives a value — here something recognisable, since what matters is that there is none.
func expanded(line string) string {
	var out strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] != '%' || i+1 == len(line):
			out.WriteByte(line[i])
		case line[i+1] == '%':
			out.WriteByte('%')
			i++
		default:
			out.WriteString("<value of %" + string(line[i+1]) + ">")
			i++
		}
	}
	return out.String()
}
