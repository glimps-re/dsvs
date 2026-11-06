package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The usage block of BETA.md comes from the help: it is not written by hand, and this test keeps
// it in line — in CI as under the pre-commit hook. inserted is the only one that knows how to replace
// the block, so comparing its output to the file is exactly what go generate would write there.
func TestTheREADMEBlockIsTheHelp(t *testing.T) {
	path := filepath.Join("..", "..", "BETA.md")
	readme, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := inserted(string(readme), asDocumented())
	if err != nil {
		t.Fatal(err)
	}
	if updated != string(readme) {
		t.Error("the usage block of BETA.md drifted from the help: go generate ./cmd/dsvs brings it back in line")
	}
}

// A README without its markers is refused, naming the missing one: generating into a file whose
// boundary is unknown would overwrite prose written by hand.
func TestAREADMEWithoutMarkersIsRefused(t *testing.T) {
	for _, text := range []string{"nothing at all", blockOpens + "\na block without an end"} {
		if _, err := inserted(text, asDocumented()); err == nil || !strings.Contains(err.Error(), "marker") {
			t.Errorf("error = %v; want the missing marker", err)
		}
	}
}

// The skill is an index: what the tool is, the table of commands, and where to ask for the rest.
// Everything comes from the table and the help text — it has no source of its own.
func TestTheSkillComesFromTheTable(t *testing.T) {
	var out bytes.Buffer
	writeSkill(&out, asDocumented())
	text := out.String()
	for _, want := range []string{"name: dsvs", "description:", "config.dsvs.ts", intro, "dsvs help [command]"} {
		if !strings.Contains(text, want) {
			t.Errorf("the skill does not carry %q", want)
		}
	}
	for _, cmd := range commands(asDocumented()) {
		if !strings.Contains(text, cmd.summary) {
			t.Errorf("the skill does not show the summary of %q", cmd.name)
		}
	}
}
