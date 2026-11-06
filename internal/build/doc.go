// Package build carries what the build knows about the binary: its version, set at link time, and its
// name, which is what keeps two dsvs apart — sockets, icon, hub, systemd unit and remembered projects
// all derive from it.
//
// # Two binaries at once
//
// Developing on a branch without losing the working dsvs is a permanent need, not a phase. The name
// of the executable is what keeps them apart, and build.sh takes it from the last segment of the
// branch: main gives dsvs, feat/daemon gives dsvs-daemon. Two different names therefore never meet.
package build
