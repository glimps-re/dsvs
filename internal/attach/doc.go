// Package attach runs the interface somewhere other than where it was asked for. The session renders
// the TUI itself, on a terminal a client lends it: the client copies no bytes and keeps no mirror of
// the state, it passes its descriptors and waits. That is what lets apps outlive the interface that
// watches them without a single piece of state having to cross a socket.
//
// # A session lives without a terminal
//
// What keeps detaching small is SCM_RIGHTS: the session runs in its systemd box and renders the TUI on
// the terminal it is lent, with no second renderer to write.
//
// A session's identity is the absolute path of its configuration: whoever knows which configuration
// they want knows where to knock, with no directory to look up.
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
