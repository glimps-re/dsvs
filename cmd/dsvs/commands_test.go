package main

import (
	"bytes"
	"strings"
	"testing"

	"dsvs/internal/build"
)

// Global options are accepted before as well as after the command: an option typed before is the
// default of the one after, and the last one typed wins. That is what makes both places equivalent
// without anyone having to choose.
func TestGlobalOptionsAreTakenBeforeAsAfterTheCommand(t *testing.T) {
	for _, tc := range []struct {
		what   string
		line   []string
		wanted shape
	}{
		{"none", []string{"state"}, shape{}},
		{"before the command", []string{"--config", "/one/dsvs.json", "state"}, shape{config: "/one/dsvs.json"}},
		{"after the command", []string{"state", "--config", "/one/dsvs.json"}, shape{config: "/one/dsvs.json"}},
		{"on both sides, the last one wins",
			[]string{"--config", "/one/dsvs.json", "state", "--config", "/two/dsvs.json"},
			shape{config: "/two/dsvs.json"}},
		{"the port before, the shift after", []string{"--port", "4999", "state", "--port-shift", "100"},
			shape{port: 4999, shift: 100}},
		{"repeatable on both sides", []string{"--watch", "one", "state", "--watch", "two"},
			shape{watch: paths{"one", "two"}}},
		{"after an argument", []string{"logs", "api", "--port", "4999"}, shape{port: 4999}},
		{"after two arguments", []string{"logs", "api", "front", "--config", "/one/dsvs.json"},
			shape{config: "/one/dsvs.json"}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got := parsed(t, tc.line)
			if got.config != tc.wanted.config || got.port != tc.wanted.port || got.shift != tc.wanted.shift {
				t.Errorf("shape = %+v; want %+v", got, tc.wanted)
			}
			if strings.Join(got.watch, ",") != strings.Join(tc.wanted.watch, ",") {
				t.Errorf("watch = %v; want %v", got.watch, tc.wanted.watch)
			}
		})
	}
}

// An app name as first argument is no longer a command: the error says which way to go, with the
// line to type — all that "unknown command" was missing.
func TestAnAppNameAsFirstArgumentSendsToStart(t *testing.T) {
	err := dispatch(&shape{}, []string{"api", "front"})
	if err == nil {
		t.Fatal("dsvs api front should be refused")
	}
	for _, want := range []string{`unknown command "api"`, build.Name() + " start api front"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, without %q", err, want)
		}
	}
}

// An unknown command is refused the same way, whatever one thought one was typing.
func TestAnUnknownCommandIsRefused(t *testing.T) {
	err := dispatch(&shape{}, []string{"reboot"})
	if err == nil || !strings.Contains(err.Error(), `unknown command "reboot"`) {
		t.Errorf("error = %v; want reboot refused", err)
	}
}

// The general help comes from the table: every command appears in it with its arguments and its
// summary, and the README and the skill come from there too.
func TestTheGeneralHelpComesFromTheTable(t *testing.T) {
	var out bytes.Buffer
	general(&out, &shape{net: "auto"})
	text := out.String()
	for _, cmd := range commands(&shape{}) {
		if !strings.Contains(text, cmd.name) {
			t.Errorf("the help does not show %q", cmd.name)
		}
		if !strings.Contains(text, cmd.summary) {
			t.Errorf("the help does not show the summary of %q", cmd.name)
		}
		if cmd.args != "" && !strings.Contains(text, cmd.name+" "+cmd.args) {
			t.Errorf("the help does not show the arguments of %q", cmd.name)
		}
	}
	// Global options apply to every command: the general help is the only place that carries them.
	for _, option := range []string{"--config", "--port", "--port-shift", "--net", "--tui", "--watch"} {
		if !strings.Contains(text, option) {
			t.Errorf("the help does not show %s", option)
		}
	}
}

// The help of a command says what it does and the options it takes, those only.
func TestTheHelpOfACommandShowsItsOwnOptions(t *testing.T) {
	var out bytes.Buffer
	if err := helping(&shape{}, &out, []string{"logs"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"dsvs logs", "-n <lines>", "-f", "--json"} {
		if !strings.Contains(text, want) {
			t.Errorf("the help of logs does not show %q; it says:\n%s", want, text)
		}
	}
	if strings.Contains(text, "--port-shift") {
		t.Error("the help of a command does not carry the global options")
	}
}

// help of a command that does not exist points to the list, rather than writing an empty help.
func TestHelpOfAnUnknownCommandSendsToTheList(t *testing.T) {
	var out bytes.Buffer
	err := helping(&shape{}, &out, []string{"reboot"})
	if err == nil || !strings.Contains(err.Error(), build.Name()+" help") {
		t.Errorf("error = %v; want a pointer to the list", err)
	}
}

// Orders about apps say which ones: names, or --all, not both — with neither they ask for nothing,
// and the silence would read as a success. reload and quit are about the whole session and want no
// name.
func TestOrdersRefuseWhatTheyCannotMean(t *testing.T) {
	for _, tc := range []struct {
		line []string
		said string
	}{
		{[]string{"start"}, "name at least one"},
		{[]string{"stop"}, "name at least one"},
		{[]string{"restart"}, "name at least one"},
		{[]string{"start", "--all", "api"}, "not both"},
		{[]string{"stop", "-a", "api"}, "not both"},
		{[]string{"reload", "now"}, "takes no name"},
		{[]string{"quit", "api"}, "takes no name"},
	} {
		t.Run(strings.Join(tc.line, " "), func(t *testing.T) {
			err := dispatch(&shape{}, tc.line)
			if err == nil || !strings.Contains(err.Error(), tc.said) {
				t.Errorf("error = %v; want %q", err, tc.said)
			}
		})
	}
}

// An option written after the arguments is an option, not a name: flag stops at the first argument,
// and parsing resumes after each one. A -- keeps its meaning, and what follows it is literal.
func TestOptionsAreTakenAfterTheArguments(t *testing.T) {
	for _, tc := range []struct {
		what  string
		line  []string
		lines int
		all   bool
		names string
	}{
		{"options first", []string{"-n", "20", "api"}, 20, false, "api"},
		{"an option after a name", []string{"api", "-n", "20"}, 20, false, "api"},
		{"between two names", []string{"api", "-n", "20", "front"}, 20, false, "api front"},
		{"a boolean after the names", []string{"api", "front", "-a"}, 0, true, "api front"},
		{"two options around a name", []string{"-a", "api", "-n", "3"}, 3, true, "api"},
		{"after --, everything is literal", []string{"--", "-n", "api"}, 0, false, "-n api"},
		{"a name, then --", []string{"api", "--", "-a"}, 0, false, "api -a"},
		{"nothing", nil, 0, false, ""},
	} {
		t.Run(tc.what, func(t *testing.T) {
			fs := newFlagSet("probe")
			n := fs.Int("n", 0, "how many `lines`")
			all := fs.Bool("a", false, "all")
			names, err := parse(fs, tc.line)
			if err != nil {
				t.Fatalf("%v: %v", tc.line, err)
			}
			if got := strings.Join(names, " "); got != tc.names {
				t.Errorf("arguments = %q; want %q", got, tc.names)
			}
			if *n != tc.lines || *all != tc.all {
				t.Errorf("-n = %d, -a = %v; want %d and %v", *n, *all, tc.lines, tc.all)
			}
		})
	}
}

// state, status, remember and forget take no argument: giving one is an error that says so, rather
// than a word nobody reads.
func TestReadingCommandsRefuseAnArgument(t *testing.T) {
	for _, name := range []string{"state", "status", "remember", "forget"} {
		if err := dispatch(&shape{}, []string{name, "api"}); err == nil ||
			!strings.Contains(err.Error(), "takes no argument") {
			t.Errorf("dsvs %s api = %v; want the argument refused", name, err)
		}
	}
}

// parsed returns the shape as this command line leaves it, without executing the command. The global
// FlagSet is parsed first, the command's next, exactly as in main.
func parsed(t *testing.T, line []string) shape {
	t.Helper()
	s := &shape{}
	fs := newFlagSet("dsvs")
	s.declare(fs)
	if err := fs.Parse(line); err != nil {
		t.Fatalf("the global options of %v: %v", line, err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		t.Fatalf("%v carries no command", line)
	}
	for _, cmd := range commands(s) {
		if cmd.name != rest[0] {
			continue
		}
		fs := newFlagSet("dsvs " + cmd.name)
		s.declare(fs)
		cmd.declare(fs)
		if _, err := parse(fs, rest[1:]); err != nil {
			t.Fatalf("the options of %s: %v", cmd.name, err)
		}
		return *s
	}
	t.Fatalf("%q is not a command", rest[0])
	return *s
}
