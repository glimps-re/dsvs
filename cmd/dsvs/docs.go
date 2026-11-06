package main

//go:generate go run . --readme ../../README.md

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// The markers bounding the generated block in the README. What lies between them comes from the
// help; the rest of the file is written by hand and never touched.
const (
	blockOpens  = "<!-- dsvs help -->"
	blockCloses = "<!-- /dsvs help -->"
)

// writeSkill writes the skill an agent loads: what the tool is, its commands, and which commands tell
// more. It is an index, not a manual — the agent asks the binary for details when it needs them,
// rather than carrying the whole context upfront. Everything it says comes from the table and the
// help text: there is no second source to keep in agreement.
func writeSkill(out io.Writer, s *shape) {
	fmt.Fprintf(out, `---
name: dsvs
description: Runs the apps of a project behind a single port. Load it when the project has a config.dsvs.ts or a dsvs.json, or when dsvs comes up — its apps, facades, sessions, or the port they answer on.
---

# dsvs — control panel for dev-servers

%s
## Commands

`, intro)
	fmt.Fprintln(out, "```")
	list(out, s)
	fmt.Fprintln(out, "```")
	fmt.Fprint(out, `
## Finding out more

This index stops here on purpose: ask the binary rather than guess. Its output is meant to be read as
much by an agent as by a human, and --json is there where a machine reads.

`)
	for _, name := range []string{"help", "state", "status", "logs", "config"} {
		if cmd, known := pick(s, name); known {
			fmt.Fprintf(out, "- `dsvs %s` — %s\n", cmd.usage(), cmd.summary)
		}
	}
	fmt.Fprintln(out, "- The README of the repository documents the configuration file itself, field by field.")
}

// refreshREADME brings the README block back in line with the help, rendered in memory: this is what
// go generate ./cmd/dsvs runs, and the pre-commit hook with it. Nothing else in the file moves, and a
// block already in line rewrites nothing.
func refreshREADME(path string) error {
	readme, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, err := inserted(string(readme), asDocumented())
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if updated == string(readme) {
		return nil
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}

// inserted returns this text with the help between its markers. The markers are the boundary: what
// surrounds them is prose written by hand, and the block they enclose belongs to the help alone.
func inserted(text string, s *shape) (string, error) {
	head, rest, found := strings.Cut(text, blockOpens)
	if !found {
		return "", fmt.Errorf("no %s marker", blockOpens)
	}
	_, tail, found := strings.Cut(rest, blockCloses)
	if !found {
		return "", fmt.Errorf("no %s marker", blockCloses)
	}
	var help strings.Builder
	general(&help, s)
	return fmt.Sprintf("%s%s\n```\n%s```\n%s%s", head, blockOpens, help.String(), blockCloses, tail), nil
}

// asDocumented is the shape whose help is written in the documents: the repository's defaults, not
// those of the generating machine — a DSVS_PORT_SHIFT set here has no business in the README.
func asDocumented() *shape { return &shape{net: "auto"} }
