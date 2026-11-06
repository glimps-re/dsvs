package build

import (
	"os"
	"path/filepath"
)

// Version is the binary's, set at build time from the git tag. An honest default: a binary built by
// hand does not pretend to be a published version.
var Version = "dev"

// Origin is the clone this binary was built from, empty when it comes from a release. It decides
// where a newer version is fetched from, and there is no hybrid mode: carried by the binary, it cannot
// disagree with what runs.
var Origin = ""

// Stable and Beta are the two published binaries. The name decides the channel a release binary
// follows and the configuration file it reads, so that the beta runs beside the stable without either
// reaching into the other.
const (
	Stable = "dsvs"
	Beta   = "dsvs-beta"
)

// Name is the name of the running executable. os.Executable resolves links: two links to the same
// binary therefore share everything derived from that name.
func Name() string {
	exe, err := os.Executable()
	if err != nil {
		return Stable
	}
	return filepath.Base(exe)
}

// Label is what is shown: the name and the version, enough to tell two binaries apart at a glance.
func Label() string { return Name() + " " + Version }
