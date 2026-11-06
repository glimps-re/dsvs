// Package attach runs the interface somewhere other than where it was asked for. The session renders
// the TUI itself, on a terminal a client lends it: the client copies no bytes and keeps no mirror of
// the state, it passes its descriptors and waits. That is what lets apps outlive the interface that
// watches them without a single piece of state having to cross a socket.
//
// # A session lives without a terminal
//
// What keeps detaching small is SCM_RIGHTS: the session runs in its systemd box and renders the TUI on
// the terminal it is lent, with no second renderer to write. The background service holds only the
// icon and the panel; the ports stay with the hub, which it does not own.
//
// A session's identity is the absolute path of its configuration: whoever knows which configuration
// they want knows where to knock, with no directory to look up.
//
// # The socket carries no version
//
// The hub carries one, this socket none, and that is deliberate: an update moves the service to the
// new binary before the sessions move themselves, so a new client talks for a while to old sessions —
// and to those that, outside a systemd box, keep their binary. That is workable because what crosses
// fits in a few verbs — start, stop, restart, reload, renew, quit, logs — written by the clients
// exactly as the session understands them. Renaming them would get them silently ignored, and that is
// the one thing not to do without thinking: reload used to mean "take the binary on disk", which the
// command line now calls renew, because a reload that replaces the binary instead of rereading the
// configuration could not be exposed as such. A session from before the rename ignores renew: the
// update bounds each handover and then says that this session keeps its binary. The state a session
// renders, for its part, gives the version it carries: that is what lets a handover be checked
// instead of trusted.
//
// # A handover is not a departure
//
// renew asks the session to move to the binary on disk. Its socket crosses the exec — it never stopped
// accepting, and Adopt picks it up on the other side — so a client has nothing to wait for to
// reattach. Attached interfaces receive the word "reloading" just before: their TUI does not cross,
// the lent terminal is handed back, and the client asks for an interface again instead of exiting with
// an error.
//
// The socket is opened before the configuration is read, and that is what also makes it the session's
// voice: a session that does not start says so there, to whoever waits for it, instead of dying on a
// silent wait.
//
// # Reading without attaching
//
// Two verbs read without lending a terminal: watch, which renders the state on every change — that is
// what the panel consumes —, and logs, which renders the tail of the logs as one line of JSON then
// closes. logs is a question, not a stream: following the logs means attaching as "plain".
//
// # The three exits of Attach
//
// A lent interface ends in three ways, and the caller tells them apart: the session handed the terminal
// back itself ("bye"), it left while holding it — the alternate screen and the cursor are then
// restored here —, or it never started and said why on its socket before going away.
package attach
