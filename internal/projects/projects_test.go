package projects

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A remembered project is read back in the order it arrived, only once whatever happens, and
// forgetting it does not touch the others.
func TestProjectsAreKeptInOrderAndOnlyOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := List(); len(got) != 0 {
		t.Fatalf("without a file, the projects are %v", got)
	}

	one, two := configAt(t, "one"), configAt(t, "two")
	for _, config := range []string{one, two, one} {
		if err := Remember(config); err != nil {
			t.Fatal(err)
		}
	}
	if got := List(); !slices.Equal(got, []string{one, two}) {
		t.Errorf("projects = %v, want both in their order of arrival", got)
	}

	if _, _, err := Forget(one); err != nil {
		t.Fatal(err)
	}
	if got := List(); !slices.Equal(got, []string{two}) {
		t.Errorf("after a forget, projects = %v", got)
	}

	// The last forget does not leave a file with one empty line, which would make a project without
	// a name.
	if _, _, err := Forget(two); err != nil {
		t.Fatal(err)
	}
	if got := List(); len(got) != 0 {
		t.Errorf("all forgotten, projects = %v", got)
	}
}

// A path carrying a line break would cut the file in two, and invent a project: it is refused
// rather than remembered askew.
func TestALineBreakNeverEntersTheList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	err := Remember(configAt(t, "one") + "\n/invented/dsvs.json")
	if err == nil || !strings.Contains(err.Error(), "line break") {
		t.Errorf("Remember = %v, want the line break refused", err)
	}
	if got := List(); len(got) != 0 {
		t.Errorf("projects = %v, want none", got)
	}
}

// Only a file that exists, named by an absolute path, is remembered: the panel does not list a
// project that does not exist.
func TestOnlyAnExistingFileIsRemembered(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	for name, config := range map[string]string{
		"relative": "dsvs.json",
		"dir":      dir,
		"absent":   filepath.Join(dir, "absent.json"),
	} {
		if err := Remember(config); err == nil {
			t.Errorf("%s: %q remembered without an error", name, config)
		}
	}
	if got := List(); len(got) != 0 {
		t.Errorf("projects = %v, want none", got)
	}
}

// A second choice adds to the first, and forgetting the project forgets everything, leaving no file
// behind.
func TestValuesAreKeptForTheProjectUntilItIsForgotten(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	config := configAt(t, "one")

	for _, values := range []map[string]string{{"env": "staging"}, {"locale": "fr"}} {
		if err := Keep(config, values); err != nil {
			t.Fatal(err)
		}
	}
	if kept := Recall(config); !maps.Equal(kept, map[string]string{"env": "staging", "locale": "fr"}) {
		t.Errorf("kept %v, want both choices", kept)
	}
	if got := files(t, state); len(got) != 2 {
		t.Errorf("files %v, want the list and the project's values: no temporary file remains", got)
	}

	if _, _, err := Forget(config); err != nil {
		t.Fatal(err)
	}
	if kept := Recall(config); len(kept) != 0 {
		t.Errorf("forgotten, the project still keeps %v", kept)
	}
	if got := files(t, state); len(got) != 0 {
		t.Errorf("forgotten, %v remains", got)
	}
}

// A value kept for a project gets it listed, like remember, forgotten or not: what is kept is seen
// and forgotten through the list. For a configuration that does not exist, nothing is kept, and
// nothing is listed.
func TestAValueKeptListsItsProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	one, two := configAt(t, "one"), configAt(t, "two")
	absent := filepath.Join(t.TempDir(), "absent.json")

	if err := Keep(absent, map[string]string{"discount": "10"}); err == nil {
		t.Error("Keep of a missing configuration kept something")
	}
	if got := List(); len(got) != 0 {
		t.Fatalf("nothing kept, projects = %v", got)
	}

	if err := Keep(one, map[string]string{"api": "staging"}); err != nil {
		t.Fatal(err)
	}
	if err := Keep(two, map[string]string{"discount": "10"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Forget(two); err != nil {
		t.Fatal(err)
	}
	if err := Keep(two, map[string]string{"discount": "5"}); err != nil {
		t.Fatal(err)
	}
	if got := List(); !slices.Equal(got, []string{one, two}) {
		t.Errorf("projects = %v, want both, in the order they kept", got)
	}
}

// A file damaged by hand keeps nothing, and is not overwritten: the error says which one.
func TestAnUnreadableFileIsNeitherReadNorOverwritten(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	config := configAt(t, "one")
	if err := Keep(config, map[string]string{"env": "staging"}); err != nil {
		t.Fatal(err)
	}
	kept, err := paramsFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if onProject := Recall(config); len(onProject) != 0 {
		t.Errorf("Recall = %v, want nothing", onProject)
	}
	if err := Keep(config, map[string]string{"env": "local"}); err == nil || !strings.Contains(err.Error(), kept) {
		t.Errorf("Keep = %v, want an error that quotes %s", err, kept)
	}
	if text, _ := os.ReadFile(kept); string(text) != "{" {
		t.Errorf("the file was overwritten: %q", text)
	}
}

// files returns the files under this directory.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			found = append(found, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func configAt(t *testing.T, name string) string {
	t.Helper()
	config := filepath.Join(t.TempDir(), name, "dsvs.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"apps":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}
