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
