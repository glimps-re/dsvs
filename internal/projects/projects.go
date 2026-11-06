// Package projects remembers the projects already seen — their configurations, from one start to
// the next. The panel shows them cold: opening a session thus returns with the list of what can be
// launched, and not with four dev-servers already launched.
//
// One absolute path per line, the line appended at the end of the file: two sessions born together
// do not lose each other, where re-reading and rewriting would. Duplicates are settled on reading,
// the order of arrival is the panel's.
package projects

import (
	"bufio"
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
// there is not rewritten, and a path that would carry a line break is refused rather than cutting the
// file in two.
func Remember(config string) error {
	if strings.ContainsAny(config, "\n\r") {
		return nil
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
// stop what lives.
func Forget(config string) error {
	file, err := path()
	if err != nil {
		return err
	}
	kept := slices.DeleteFunc(List(), func(other string) bool { return other == config })
	if len(kept) == 0 {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(file, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}

// path is the projects file. It lives under the binary's name, like its sockets: two dsvs binaries
// do not fight over the same memory.
func path() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, build.Name(), "projects"), nil
}
