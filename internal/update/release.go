package update

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"dsvs/internal/attach"
	"dsvs/internal/build"
	"dsvs/internal/hub"
)

// downloading bounds the download of a binary, which weighs megabytes where every other request
// weighs a page.
const downloading = 5 * time.Minute

// InstallBeta puts dsvs-beta beside this binary, at the newest version of the beta channel, then
// writes its unit and starts its service: a second icon, amber, comes up next to this one. It
// returns what the beta said once installed.
func InstallBeta(step func(string)) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return installBeta(exe, step)
}

// BetaToInstall is the beta dsvs can put beside it, empty when there is none to offer: no
// pre-release ahead of the stable, a dsvs-beta already there, or a binary that is not dsvs — the beta
// itself, or one built from a branch.
func BetaToInstall() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return betaToInstall(exe)
}

func betaToInstall(exe string) (string, error) {
	if filepath.Base(exe) != build.Stable {
		return "", nil
	}
	if _, err := os.Stat(betaBeside(exe)); err == nil {
		return "", nil
	}
	version, err := newest(true)
	if err != nil || !prerelease(version) {
		return "", err
	}
	return version, nil
}

// RemoveBeta takes the beta off this machine: its sessions close, its unit and its binary go, and
// its service leaves last — the panel that asked lives in it. Only dsvs-beta removes itself this way,
// and nothing it touches carries another name: the stable beside it stays as it was.
func RemoveBeta(step func(string)) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return removeBeta(exe, step)
}

func installBeta(stable string, step func(string)) (string, error) {
	version, err := newest(true)
	if err != nil {
		return "", err
	}
	beta := betaBeside(stable)
	if err := fetch(version, beta, step); err != nil {
		return "", err
	}
	step("checking " + build.Beta)
	if _, err := VersionOf(beta); err != nil {
		os.Remove(beta)
		return "", fmt.Errorf("%s %s does not run, and was removed: %w", build.Beta, version, err)
	}
	step("starting its service")
	// The port shift in this environment is this binary's unit speaking, not the beta's: the beta's own
	// configuration declares its port.
	out, err := runWith(nil, "", beta, "--port-shift=0", "--install-unit")
	if err != nil {
		return "", fmt.Errorf("%s --install-unit: %w%s", build.Beta, err, said(lastLine(out)))
	}
	return build.Beta + " " + version + " installed: " + lastLine(out), nil
}

func removeBeta(beta string, step func(string)) error {
	if filepath.Base(beta) != build.Beta {
		return fmt.Errorf("%s is not the beta: only %s uninstalls itself", filepath.Base(beta), build.Beta)
	}
	if living, _ := hub.Sessions(); len(living) > 0 {
		step(fmt.Sprintf("closing %d session(s)", len(living)))
		for _, session := range living {
			closeSession(session.Config)
		}
	}
	step("removing " + build.Beta + ".service")
	if out, err := runWith(nil, "", beta, "--remove-unit"); err != nil {
		return fmt.Errorf("%s --remove-unit: %w%s", build.Beta, err, said(lastLine(out)))
	}
	step("removing " + beta)
	for _, file := range []string{beta, beta + ".old", beta + ".new", beta + ".lock"} {
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if conn, err := net.Dial("unix", hub.TrayAddr()); err == nil {
		step("stopping its service")
		fmt.Fprintln(conn, "quit")
		conn.Close()
	}
	return nil
}

// closeSession orders a session to close and waits for its link to drop, which is the
// acknowledgement. One still stopping its apps after a while is left to it: it is on its way out, and
// the binary it runs can go from under it.
func closeSession(config string) {
	link, err := attach.Watch(config)
	if err != nil {
		return
	}
	defer link.Close()
	if link.Order("quit") != nil {
		return
	}
	waited := time.After(passing)
	for {
		select {
		case _, live := <-link.States():
			if !live {
				return
			}
		case <-waited:
			return
		}
	}
}

func betaBeside(stable string) string { return filepath.Join(filepath.Dir(stable), build.Beta) }

// fromRelease puts in place the newest version of the channel this binary follows.
func fromRelease(step func(string), exe string) error {
	version, err := published()
	if err != nil {
		return err
	}
	if version == "" {
		return errors.New("already the latest published version")
	}
	return fetch(version, exe, step)
}

// published is the newest version of the channel this binary follows, empty when it is the one
// running.
func published() (string, error) {
	version, err := newest(build.Name() == build.Beta)
	if err != nil || version == build.Version {
		return "", err
	}
	return version, nil
}

// releases is where the published versions live. DSVS_RELEASES points at a mirror laid out the same
// way: <base>/latest redirects to <base>/tag/<version>, <base>.atom lists the recent ones, and
// <base>/download/<version>/<file> serves a file.
func releases() string {
	if base := os.Getenv("DSVS_RELEASES"); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return "https://github.com/glimps-re/dsvs/releases"
}

// newest is the newest version of a channel. The stable one is what GitHub calls the latest release,
// never a pre-release. The beta channel takes the highest of the recent ones, stable or not: a stable
// that overtook the last beta is what the beta runs until the next one.
//
// Neither goes through the GitHub API: unauthenticated, it allows sixty requests an hour to an
// address an office shares, and the panel asks each time it opens.
func newest(betas bool) (string, error) {
	if !betas {
		answer, err := get(releases()+"/latest", patience)
		if err != nil {
			return "", err
		}
		answer.Body.Close()
		landed := answer.Request.URL.Path
		if path.Base(path.Dir(landed)) != "tag" {
			return "", fmt.Errorf("no release is published at %s", releases())
		}
		return path.Base(landed), nil
	}
	answer, err := get(releases()+".atom", patience)
	if err != nil {
		return "", err
	}
	defer answer.Body.Close()
	var feed struct {
		Entries []struct {
			Link struct {
				Href string `xml:"href,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}
	if err := xml.NewDecoder(answer.Body).Decode(&feed); err != nil {
		return "", fmt.Errorf("%s.atom: %w", releases(), err)
	}
	var versions []string
	for _, entry := range feed.Entries {
		version, err := url.PathUnescape(path.Base(entry.Link.Href))
		if _, _, ok := semverParts(version); err == nil && ok {
			versions = append(versions, version)
		}
	}
	slices.SortFunc(versions, compareSemver)
	missing := fmt.Errorf("no release is published at %s", releases())
	// The feed lists a tag before its release carries anything — while it is being built, for one: the
	// version to take is the highest that has its files.
	for _, version := range slices.Backward(versions) {
		sums, err := get(asset(version, "checksums.txt"), patience)
		if err == nil {
			sums.Body.Close()
			return version, nil
		}
		missing = err
	}
	return "", missing
}

// fetch puts at target the binary this release carries for this machine. Its checksum is checked
// against the release's before anything moves: a binary that does not match, or that no checksum
// vouches for, never takes anyone's place. It is written beside target then renamed, so that a
// half-downloaded binary is never the one that runs.
func fetch(version, target string, step func(string)) error {
	binary := "dsvs-linux-" + runtime.GOARCH
	step("downloading " + version)
	sums, err := get(asset(version, "checksums.txt"), patience)
	if err != nil {
		return fmt.Errorf("the checksums of %s are out of reach, refusing %s: %w", version, binary, err)
	}
	wanted := checksumOf(sums.Body, binary)
	sums.Body.Close()
	if wanted == "" {
		return fmt.Errorf("%s publishes no checksum for %s: refusing it", version, binary)
	}
	answer, err := get(asset(version, binary), downloading)
	if err != nil {
		return err
	}
	defer answer.Body.Close()
	coming := target + ".new"
	written, err := os.OpenFile(coming, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(written, sum), answer.Body)
	if closed := written.Close(); err == nil {
		err = closed
	}
	if err == nil && hex.EncodeToString(sum.Sum(nil)) != wanted {
		err = fmt.Errorf("%s of %s does not match its published checksum: refusing it", binary, version)
	}
	if err != nil {
		os.Remove(coming)
		return err
	}
	step("installing " + version)
	return os.Rename(coming, target)
}

func asset(version, file string) string { return releases() + "/download/" + version + "/" + file }

// get asks for a page and hands it over only when it is there: any other status is an error that
// says what was asked and what came back.
func get(address string, within time.Duration) (*http.Response, error) {
	answer, err := (&http.Client{Timeout: within}).Get(address)
	if err != nil {
		return nil, err
	}
	if answer.StatusCode != http.StatusOK {
		answer.Body.Close()
		return nil, fmt.Errorf("%s: %s", address, answer.Status)
	}
	return answer, nil
}

// checksumOf reads a sha256sum listing and returns the checksum given for this file, empty when
// there is none.
func checksumOf(listing io.Reader, file string) string {
	lines := bufio.NewScanner(listing)
	for lines.Scan() {
		fields := bytes.Fields(lines.Bytes())
		if len(fields) == 2 && string(bytes.TrimPrefix(fields[1], []byte("*"))) == file {
			return strings.ToLower(string(fields[0]))
		}
	}
	return ""
}

func prerelease(version string) bool {
	_, pre, ok := semverParts(version)
	return ok && len(pre) > 0
}

// compareSemver orders two semantic versions, "v" prefix included, the way semver.org does: numbers
// compare as numbers, and a pre-release comes before its release. Both are versions: semverParts says
// which are.
func compareSemver(a, b string) int {
	aCore, aPre, _ := semverParts(a)
	bCore, bPre, _ := semverParts(b)
	if order := slices.Compare(aCore[:], bCore[:]); order != 0 {
		return order
	}
	if len(aPre) == 0 || len(bPre) == 0 {
		return cmp.Compare(len(bPre), len(aPre))
	}
	return slices.CompareFunc(aPre, bPre, compareIdentifiers)
}

func semverParts(version string) (core [3]int, pre []string, ok bool) {
	version, prefixed := strings.CutPrefix(version, "v")
	version, _, _ = strings.Cut(version, "+")
	version, preText, hasPre := strings.Cut(version, "-")
	numbers := strings.Split(version, ".")
	if !prefixed || len(numbers) != 3 {
		return core, nil, false
	}
	for i, number := range numbers {
		n, err := strconv.Atoi(number)
		if err != nil || n < 0 {
			return core, nil, false
		}
		core[i] = n
	}
	if hasPre {
		pre = strings.Split(preText, ".")
	}
	return core, pre, true
}

func compareIdentifiers(a, b string) int {
	aNumber, aErr := strconv.Atoi(a)
	bNumber, bErr := strconv.Atoi(b)
	switch {
	case aErr == nil && bErr == nil:
		return cmp.Compare(aNumber, bNumber)
	case aErr == nil:
		return -1
	case bErr == nil:
		return 1
	}
	return strings.Compare(a, b)
}
