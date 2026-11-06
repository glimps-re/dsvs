package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The role is the decision point of the command: set up wrong, it had made a role of the table
// unreachable. A flag names the role, wherever it sits in the table; none leaves the default role, the
// one that reads the command line.
func TestOnlyPicksTheRoleAsked(t *testing.T) {
	for _, tc := range []struct {
		what   string
		asked  []string
		chosen string
	}{
		{"no flag", nil, "default"},
		{"the first of the table", []string{"hub"}, "hub"},
		{"the last of the table", []string{"session"}, "session"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			chosen, err := only(asking(tc.asked...), named("default"))
			if err != nil {
				t.Fatalf("only = %v, want a role", err)
			}
			if got := chosen(); got.Error() != tc.chosen {
				t.Errorf("role %v; want %s", got, tc.chosen)
			}
		})
	}
}

// Two roles at once are refused by naming them, where the order of the lines would decide silently.
func TestOnlyRefusesTwoRolesByNamingThem(t *testing.T) {
	chosen, err := only(asking("hub", "session"), named("default"))
	if chosen != nil {
		t.Error("only returned a role for two flags")
	}
	if err == nil || !strings.Contains(err.Error(), "--hub") || !strings.Contains(err.Error(), "--session") {
		t.Errorf("error = %v; want both flags named", err)
	}
}

// asking returns a table of roles reduced to their name, the given ones asked for. Each returns its
// name as an error: that is what tells which one only chose.
func asking(asked ...string) []role {
	var roles []role
	for _, name := range []string{"hub", "tray", "session"} {
		roles = append(roles, role{name, slices.Contains(asked, name), named(name)})
	}
	return roles
}

func named(name string) func() error {
	return func() error { return errors.New(name) }
}
