package projects

import (
	"slices"
	"testing"
)

// A remembered project is read back in the order it arrived, only once whatever happens, and
// forgetting it does not touch the others.
func TestProjectsAreKeptInOrderAndOnlyOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := List(); len(got) != 0 {
		t.Fatalf("without a file, the projects are %v", got)
	}

	for _, config := range []string{"/one/dsvs.json", "/two/dsvs.json", "/one/dsvs.json"} {
		if err := Remember(config); err != nil {
			t.Fatal(err)
		}
	}
	if got := List(); !slices.Equal(got, []string{"/one/dsvs.json", "/two/dsvs.json"}) {
		t.Errorf("projects = %v, want both in their order of arrival", got)
	}

	if err := Forget("/one/dsvs.json"); err != nil {
		t.Fatal(err)
	}
	if got := List(); !slices.Equal(got, []string{"/two/dsvs.json"}) {
		t.Errorf("after a forget, projects = %v", got)
	}

	// The last forget does not leave a file with one empty line, which would make a project without
	// a name.
	if err := Forget("/two/dsvs.json"); err != nil {
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
	if err := Remember("/one/dsvs.json\n/invented/dsvs.json"); err != nil {
		t.Fatal(err)
	}
	if got := List(); len(got) != 0 {
		t.Errorf("projects = %v, want none", got)
	}
}
