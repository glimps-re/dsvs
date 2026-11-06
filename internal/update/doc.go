// Package update replaces the binary with a newer version. The provenance is the one the build
// stamped — the clone it comes from, or nothing, and then it is the releases: there is no hybrid mode,
// and so nothing to guess here.
//
// # What an update does not lose
//
// This is not a hot swap, and it must not be called one: it is an update without service
// interruption. Nothing goes down, no app dies, and the open sessions move to the new binary without
// losing one — each execs onto itself and adopts its apps, which never stopped being its children.
//
// The sessions go before the service, and the order matters: the panel lives in the service, which
// vanishes in its exec, and whatever came after would have no one left to report to. None is taken at
// its word — the one that came back is joined and asked which version it carries. One that refused
// keeps its apps and says so in its log; outside a systemd box, that is the normal case.
//
// The rest is measured rather than promised. Replacing the file touches no live process, provided it
// is replaced and not overwritten: coreutils install lays down a new inode, so does a rename, and the
// old one stays mounted for whoever executes it — no ETXTBSY, no SIGBUS. cp, on the other hand, writes
// into the existing file and fails with ETXTBSY on a running binary (measured); the install path
// therefore only uses install and mv. os.Executable then names the new binary: Go returns the path
// without the "(deleted)" suffix that /proc/self/exe carries, so an old process that restarts starts
// the updated one.
//
// The apps die with their session, through its cgroup: restarting a session costs its apps, and that
// is exactly what is not done — the session keeps its PID, hence its cgroup. The service only holds
// its icon and replaces itself by the same exec. The hub moves to the new binary on its own, leaving
// with its last session: the update does not kill it, it has no need to.
//
// The only scenario that still costs the apps is a version that changes the dialogue between a session
// and the hub. The old session re-registering with a new hub is refused, and its apps then run with
// nothing answering; closing the sessions before updating becomes mandatory again that day.
//
// # One update at a time
//
// A flock on the shared resource: the clone when the binary comes from one (.update.lock at its root),
// the binary itself otherwise (<binary>.lock). Two updates in parallel would keep as .old the binary
// the other one just laid down, and two binaries born from the same clone would merge and build there
// at the same time. A double click in the panel and a dsvs update in the terminal fall under the same
// rule.
//
// A modified clone does not update: git describe --dirty marks every dirty state with the same suffix,
// so nothing would tell the version taken from the one that runs. build.sh --install is what serves in
// that case.
//
// # Where the binary lands, and what decides
//
// build.sh --install puts the binary at the destination DSVS_TARGET names, and the update sets it to
// the full path of the running binary. Failing that, it is build.sh's default directory and the name
// taken from the branch. A moved binary therefore replaces itself instead of seeing the new one land
// beside it.
//
// The name, though, does not move: build.sh refuses to install under a name that is not the one its
// branch gives. The name comes from the branch and the destination comes from the update; without
// this rule, a clone left on another branch builds that one and puts it under the name of the binary
// being updated, which from then on carries code nobody asked of it.
//
// The binary on disk alone decides: build.sh ends by running what it just installed, so an install can
// fail after replacing the binary. The disk is checked in every case — the new one in place counts as
// an update done, the old one in place points to the reason for the failure. Measured, this is what
// rollback was missing.
//
// The restart of the service is acknowledged by its connection dropping: a successful exec carries it
// off without a word, and a word is precisely what kept it from leaving. A dropped connection is not
// taken at its word either — someone answering behind the icon's socket is checked again.
//
// # Releases, and the two channels
//
// A binary without a clone downloads from the public releases over HTTPS, with neither account nor
// token. DSVS_RELEASES points at a mirror laid out the same way: <base>/latest redirects to the latest
// stable, <base>.atom lists the recent releases, <base>/download/<version>/<file> serves a file. None
// of it is the GitHub API, which allows sixty unauthenticated requests an hour per address — an office
// shares one, and the panel asks each time it opens. The feed also lists a tag before its release
// carries anything, while it is being built: the version taken is the highest that has its files.
//
// Every release carries a checksums.txt in sha256sum format, and the binary is checked against it
// before it takes any place. A wrong checksum, or none, is a refusal, and the binary that runs stays.
//
// The channel is the binary's name, like everything else it derives. dsvs follows the stable
// releases. dsvs-beta follows the highest version published, pre-releases included, which is also a
// stable that overtook the last beta. Any other name — a binary built from a branch — follows the
// stable releases when it has no clone.
//
// dsvs-beta lives beside dsvs, in the same directory, and the stable stays its way back. dsvs installs
// it from its panel when a beta is out and none is there: the binary lands beside it, then writes its
// own unit, named after itself. The beta uninstalls itself from its own panel: its sessions close, its
// unit and binary go, and its service leaves last since the panel lives in it. Neither path touches
// anything that carries the other name.
package update
