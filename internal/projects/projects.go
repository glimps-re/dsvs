// Package projects remembers the projects already seen — their configurations, from one start to
// the next. The panel shows them cold: opening a session thus returns with the list of what can be
// launched, and not with four dev-servers already launched.
//
// One absolute path per line, the line appended at the end of the file: two sessions born together
// do not lose each other, where re-reading and rewriting would. Duplicates are settled on reading,
// the order of arrival is the panel's.
//
// A project also keeps the values chosen for its parameters, where its form says so (keep), a new
// session included. One file per project, rewritten whole through a rename: only one session holds a
// project under a given binary, so there is only one writer per file. Keeping a value remembers the project, and forgetting the project forgets its values: what
// is kept for a project is always seen and forgotten through the list.
package projects

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"dsvs/internal/build"
)

// List returns the remembered projects, in the order they arrived. No file to read, no projects: it
// is not an error, it is the answer.
func List() []string {
	kept, err := path()
	if err != nil {
		return nil
	}
	file, err := os.Open(kept)
	if err != nil {
		return nil
	}
	defer file.Close()
	var out []string
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		config := strings.TrimSpace(lines.Text())
		if config != "" && !slices.Contains(out, config) {
			out = append(out, config)
		}
	}
	return out
}

// Remember remembers this project. The gesture can be repeated harmlessly: a configuration already
// there is not rewritten. It is the sole guardian of the file: a relative path, one carrying a line
// break (it would cut the file in two), a missing one or one that names a directory is refused with
// an error, and what the configuration contains is not evaluated.
func Remember(config string) error {
	if err := checkRememberable(config); err != nil {
		return err
	}
	if slices.Contains(List(), config) {
		return nil
	}
	kept, err := path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(kept, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(config + "\n")
	return err
}

// Forget forgets this project. What runs for it carries on: one forgets an address, one does not
// stop what lives. The values kept for it are forgotten with it. It returns what it forgot — its
// place in the list, its values.
func Forget(config string) (listed, valued bool, err error) {
	file, err := path()
	if err != nil {
		return false, false, err
	}
	values, err := paramsFile(config)
	if err != nil {
		return false, false, err
	}
	switch err := os.Remove(values); {
	case err == nil:
		valued = true
	case !os.IsNotExist(err):
		return false, false, err
	}
	all := List()
	if !slices.Contains(all, config) {
		return false, valued, nil
	}
	kept := slices.DeleteFunc(all, func(other string) bool { return other == config })
	if len(kept) == 0 {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return true, valued, err
		}
		return true, valued, nil
	}
	return true, valued, os.WriteFile(file, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}

// Recall returns the values kept for this project, in a table always ready to receive other values.
// No file, or an unreadable file: nothing is kept, and it is not an error.
func Recall(config string) (kept map[string]string) {
	_, memory, err := memoryOf(config)
	kept = map[string]string{}
	if err == nil {
		maps.Copy(kept, memory.Kept)
	}
	return kept
}

// Keep keeps these values for this project, over those that were there.
func Keep(config string, values map[string]string) error {
	return rewrite(config, func(memory *memory) {
		if memory.Kept == nil {
			memory.Kept = map[string]string{}
		}
		maps.Copy(memory.Kept, values)
	})
}

// memory is what a project keeps of its parameters.
type memory struct {
	Kept map[string]string `json:"kept,omitzero"`
}

// rewrite is the sole guardian of the values file: it remembers the project, re-reads the file,
// applies the change to it, and replaces it in one go. An unreadable file is not overwritten, the
// error quotes its path.
func rewrite(config string, change func(*memory)) error {
	if err := Remember(config); err != nil {
		return err
	}
	file, memory, err := memoryOf(config)
	if err != nil {
		return err
	}
	change(&memory)
	text, err := json.Marshal(memory)
	if err != nil {
		return err
	}
	return writeAtomically(file, text)
}

// memoryOf reads what is kept for this project, and returns the file it comes from: an empty memory
// when there is none.
func memoryOf(config string) (string, memory, error) {
	var kept memory
	file, err := paramsFile(config)
	if err != nil {
		return "", kept, err
	}
	text, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return file, kept, nil
	}
	if err != nil {
		return "", kept, err
	}
	if err := json.Unmarshal(text, &kept); err != nil {
		return "", kept, fmt.Errorf("%s: %w", file, err)
	}
	return file, kept, nil
}

// writeAtomically replaces the file in one go: a temporary file in the same directory, renamed over
// it. Whoever reads at the same time sees the old content or the new one, never half of one.
func writeAtomically(file string, text []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(file), ".*.json")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(text); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), file)
}

func checkRememberable(config string) error {
	if !filepath.IsAbs(config) {
		return fmt.Errorf("%q is not an absolute path", config)
	}
	if strings.ContainsAny(config, "\n\r") {
		return fmt.Errorf("%q holds a line break, which would cut the list in two", config)
	}
	info, err := os.Stat(config)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no configuration at %s", config)
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not a configuration", config)
	}
	return nil
}

// path is the projects file.
func path() (string, error) {
	dir, err := stateDir()
	return filepath.Join(dir, "projects"), err
}

// paramsFile is this project's values file, under its configuration's path reproduced as is:
// readable, and unique without encoding anything.
func paramsFile(config string) (string, error) {
	dir, err := stateDir()
	return filepath.Join(dir, "params", config+".json"), err
}

// stateDir is what this binary remembers. It lives under its name, like its sockets: two dsvs
// binaries do not fight over the same memory.
func stateDir() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, build.Name()), nil
}
