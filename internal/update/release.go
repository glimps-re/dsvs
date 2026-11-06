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
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"dsvs/internal/build"
)

// downloading bounds the download of a binary, which weighs megabytes where every other request
// weighs a page.
const downloading = 5 * time.Minute

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
	version, err := newest(false)
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
