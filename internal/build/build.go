package build

import (
	"os"
	"path/filepath"
)

// Version is the binary's, set at build time from the git tag. An honest default: a binary built by
// hand does not pretend to be a published version.
var Version = "dev"

// Name is the name of the running executable. os.Executable resolves links: two links to the same
// binary therefore share everything derived from that name.
func Name() string {
	exe, err := os.Executable()
	if err != nil {
		return "dsvs"
	}
	return filepath.Base(exe)
}

// Label is what is shown: the name and the version, enough to tell two binaries apart at a glance.
func Label() string { return Name() + " " + Version }
