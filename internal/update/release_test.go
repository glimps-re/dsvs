package update

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"dsvs/internal/build"
)

// Without a clone, the new version is the one the stable channel publishes: never a pre-release,
// however recent.
func TestApplyTakesTheLatestStableRelease(t *testing.T) {
	runtimeDir(t)
	exe := filepath.Join(t.TempDir(), "dsvs")
	binary(t, exe, "v0.1.0")
	from(t, "", "v0.1.0")
	publish(t, "v0.1.0", "v0.2.0", "v0.3.0-beta.1")

	if err := apply(exe, func(string) {}); err != nil {
		t.Fatalf("apply = %v", err)
	}
	if got := versionOf(t, exe); got != "v0.2.0" {
		t.Errorf("the binary in place says %q, wanted the latest stable", got)
	}
}

// A binary whose checksum is wrong, or that no checksum vouches for, never takes the place of the one
// running: the refusal says why, and nothing is left behind.
func TestApplyRefusesABinaryNoChecksumVouchesFor(t *testing.T) {
	for what, spoil := range map[string]func(shelf){
		"a wrong checksum":    func(s shelf) { s["v0.2.0/"+machineBinary()] = []byte("#!/bin/sh\necho tampered\n") },
		"no checksum file":    func(s shelf) { delete(s, "v0.2.0/checksums.txt") },
		"no line for it here": func(s shelf) { s["v0.2.0/checksums.txt"] = []byte("0123  dsvs-linux-other\n") },
	} {
		t.Run(what, func(t *testing.T) {
			runtimeDir(t)
			dir := t.TempDir()
			exe := filepath.Join(dir, "dsvs")
			binary(t, exe, "v0.1.0")
			from(t, "", "v0.1.0")
			spoil(publish(t, "v0.1.0", "v0.2.0"))

			err := apply(exe, func(string) {})
			if err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("apply = %v; want a refusal", err)
			}
			if got := versionOf(t, exe); got != "v0.1.0" {
				t.Errorf("the binary in place says %q; want the one that ran", got)
			}
			if _, err := os.Stat(exe + ".new"); err == nil {
				t.Error("the refused download is left beside the binary")
			}
		})
	}
}

// Nothing published is an answer that says where it looked, not a version to take.
func TestNewerSaysWhenNothingIsPublished(t *testing.T) {
	from(t, "", "v0.1.0")
	publish(t)

	newer, err := Newer()
	if newer != "" || err == nil || !strings.Contains(err.Error(), "no release is published") {
		t.Errorf("Newer = %q, %v; want no release said", newer, err)
	}
}

// The beta channel takes the highest version published, stable or not: pre-releases in their
// numeric order, and a stable that overtook them.
func TestTheBetaChannelTakesTheHighestVersion(t *testing.T) {
	cases := []struct {
		published []string
		wanted    string
	}{
		{[]string{"v0.1.0", "v0.2.0-beta.9", "v0.2.0-beta.10"}, "v0.2.0-beta.10"},
		{[]string{"v0.2.0-beta.2", "v0.2.0", "v0.1.1"}, "v0.2.0"},
		{[]string{"v0.3.0-beta.1", "v0.2.1"}, "v0.3.0-beta.1"},
	}
	for _, c := range cases {
		publish(t, c.published...)
		if got, err := newest(true); err != nil || got != c.wanted {
			t.Errorf("%v: newest = %q, %v; want %s", c.published, got, err, c.wanted)
		}
	}
}

// A tag shows in the feed before its release carries its files: the channel keeps to the highest
// version it can take.
func TestTheBetaChannelSkipsATagWithoutItsFiles(t *testing.T) {
	building := publish(t, "v0.1.0", "v0.2.0-beta.1")
	delete(building, "v0.2.0-beta.1/checksums.txt")
	if got, err := newest(true); err != nil || got != "v0.1.0" {
		t.Errorf("newest = %q, %v; want v0.1.0", got, err)
	}
}

// dsvs offers the beta when one is out ahead of the stable and none is beside it yet. The other
// binaries offer nothing: the beta itself, and the ones built from a branch.
func TestOnlyDsvsWithoutABetaBesideOffersOne(t *testing.T) {
	dir := t.TempDir()
	stable := filepath.Join(dir, build.Stable)
	publish(t, "v0.1.0", "v0.2.0-beta.1")

	if got, err := betaToInstall(stable); err != nil || got != "v0.2.0-beta.1" {
		t.Errorf("dsvs alone: %q, %v; want the beta", got, err)
	}
	for _, other := range []string{build.Beta, "dsvs-next"} {
		if got, err := betaToInstall(filepath.Join(dir, other)); err != nil || got != "" {
			t.Errorf("%s: %q, %v; want nothing to offer", other, got, err)
		}
	}
	binary(t, filepath.Join(dir, build.Beta), "v0.2.0-beta.1")
	if got, err := betaToInstall(stable); err != nil || got != "" {
		t.Errorf("dsvs with a beta beside: %q, %v; want nothing to offer", got, err)
	}

	publish(t, "v0.2.0-beta.1", "v0.2.0")
	os.Remove(filepath.Join(dir, build.Beta))
	if got, err := betaToInstall(stable); err != nil || got != "" {
		t.Errorf("a stable ahead of every beta: %q, %v; want nothing to offer", got, err)
	}
}

// Installing the beta puts dsvs-beta beside the stable, at the beta's version, and has it write its
// own unit — with no port shift taken from the stable's environment.
func TestInstallBetaPutsItBesideTheStable(t *testing.T) {
	runtimeDir(t)
	dir := t.TempDir()
	stable := filepath.Join(dir, build.Stable)
	binary(t, stable, "v0.1.0")
	publish(t, "v0.1.0", "v0.2.0-beta.1")

	said, err := installBeta(stable, func(string) {})
	if err != nil {
		t.Fatalf("installBeta = %v", err)
	}
	beta := filepath.Join(dir, build.Beta)
	if got := versionOf(t, beta); got != "v0.2.0-beta.1" {
		t.Errorf("dsvs-beta says %q", got)
	}
	if !strings.Contains(said, "v0.2.0-beta.1") {
		t.Errorf("installBeta said %q; want the version", said)
	}
	if got := versionOf(t, stable); got != "v0.1.0" {
		t.Errorf("the stable says %q; want it untouched", got)
	}
}

// Uninstalling the beta removes its unit through it, then the binary and what updates left beside it;
// the stable stays. Any binary but the beta refuses: this path never removes the stable.
func TestRemoveBetaLeavesTheStable(t *testing.T) {
	runtimeDir(t)
	dir := t.TempDir()
	stable := filepath.Join(dir, build.Stable)
	binary(t, stable, "v0.1.0")
	beta := filepath.Join(dir, build.Beta)
	asked := filepath.Join(dir, "asked")
	if err := os.WriteFile(beta, []byte("#!/bin/sh\necho \"$@\" >"+asked+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary(t, beta+".old", "v0.2.0-beta.1")

	if err := removeBeta(stable, func(string) {}); err == nil {
		t.Error("removeBeta took the stable")
	}
	if err := removeBeta(beta, func(string) {}); err != nil {
		t.Fatalf("removeBeta = %v", err)
	}
	if said, _ := os.ReadFile(asked); strings.TrimSpace(string(said)) != "--remove-unit" {
		t.Errorf("the beta was asked %q; want --remove-unit", said)
	}
	left, _ := filepath.Glob(filepath.Join(dir, build.Beta+"*"))
	if len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
	if got := versionOf(t, stable); got != "v0.1.0" {
		t.Errorf("the stable says %q; want it untouched", got)
	}
}

// shelf is what a fake release server serves, by "<version>/<file>".
type shelf map[string][]byte

// publish serves these versions as releases, through DSVS_RELEASES: each carries a binary for this
// machine that says its version, and a checksums.txt that vouches for it. The latest stable is the
// highest version without a pre-release. The shelf is returned for a test to spoil.
func publish(t *testing.T, versions ...string) shelf {
	t.Helper()
	files := shelf{}
	latest := ""
	for _, version := range versions {
		body := []byte("#!/bin/sh\necho " + version + "\n")
		files[version+"/"+machineBinary()] = body
		files[version+"/checksums.txt"] = fmt.Appendf(nil, "%x  %s\n", sha256.Sum256(body), machineBinary())
		if !prerelease(version) && (latest == "" || compareSemver(version, latest) > 0) {
			latest = version
		}
	}
	mux := http.NewServeMux()
	// As GitHub does, a repository without a release sends /latest to the list of releases.
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if latest == "" {
			http.Redirect(w, r, "/releases", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
	})
	mux.HandleFunc("GET /releases", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /releases/tag/{version}", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /releases.atom", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom">`)
		for _, version := range slices.Backward(versions) {
			fmt.Fprintf(w, `<entry><link rel="alternate" type="text/html" href="http://%s/releases/tag/%s"/></entry>`, r.Host, version)
		}
		fmt.Fprint(w, `</feed>`)
	})
	mux.HandleFunc("GET /releases/download/{version}/{file}", func(w http.ResponseWriter, r *http.Request) {
		body, found := files[r.PathValue("version")+"/"+r.PathValue("file")]
		if !found {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv("DSVS_RELEASES", server.URL+"/releases")
	return files
}

func machineBinary() string { return "dsvs-linux-" + runtime.GOARCH }

func versionOf(t *testing.T, exe string) string {
	t.Helper()
	version, err := VersionOf(exe)
	if err != nil {
		t.Fatalf("%s --version: %v", exe, err)
	}
	return version
}

// runtimeDir gives the test its own sockets: the beta's sessions and service are looked for there,
// and never among those of the machine.
func runtimeDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}
