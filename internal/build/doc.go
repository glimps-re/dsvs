// Package build carries what the build knows about the binary: its version, set at link time, and its
// name, which is what keeps two dsvs apart — sockets, icon, hub, systemd unit and remembered projects
// all derive from it.
//
// # Two binaries at once
//
// Developing on a branch without losing the working dsvs is a permanent need, not a phase. The name
// of the executable is what keeps them apart, and build.sh takes it from the last segment of the
// branch: main gives dsvs, feat/daemon gives dsvs-daemon. Two different names therefore never meet.
//
// The port, though, is global and cannot be shared. The second binary refuses and says everything:
// which binary holds the port, which version, since when, and for which session. Running two binaries
// on the same projects therefore requires shifting the port of one of them: the shift is chosen at
// install time and lives in the unit of its service, which sets it in the environment of everything
// it starts. Never automatic, or the facades would answer somewhere other than where they are typed.
// No relay between binaries either, which would let a binary under construction break the working
// service.
//
// The name also decides what is shown: only dsvs paints the bars of its icon white, any other binary
// paints them amber. Hovering gives the full name and the version, and the panel repeats it in its
// header. That is what Label is for.
//
// The name also decides what a published binary follows and reads: dsvs the stable releases and
// config.dsvs.ts, dsvs-beta the pre-releases too and its own config.beta.dsvs.ts.
//
// # Provenance
//
// It is carried by the binary, not by a state file: build.sh injects the path of the clone, the CI
// injects nothing. It therefore cannot disagree with what runs, and there is nothing to migrate.
package build
