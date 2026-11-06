// Package core drives the declared apps: one supervisor per app, sole owner of its process, which
// reports its state as a snapshot and its output as a ring of lines. The port, on the other hand,
// belongs to the session: taken before the interface, released on exit, and every facade answers on
// it without holding any state of its own.
//
// # Ready means ready to serve
//
// The probe does not wait for an open port, it waits for a real HTTP response on the path through
// which a facade serves the app. Measured on an Angular dev-server: port open in two seconds, real
// response at forty. A listening port would therefore have declared ready, for thirty-eight
// seconds, an app to which the ingress was sending requests it could not honor.
//
// Detecting "ready" by reading the logs was ruled out for that reason: the HTTP probe works for any
// app, whereas each runner announces its readiness its own way.
//
// # Logs are virtual terminals
//
// Each app gets a real pty, and its log interprets the escape sequences: it is the only way to
// display what the program meant to show. Dropping the cursor-positioning sequences was the first
// design, for two versions — spinners stacked one line per frame.
//
// Ruled out by measurement, not to be retried: aymanbagabas/go-pty, which keeps the pty slave open
// on the parent side and therefore blocks restarts, and golang.org/x/term/xpty, which only adds
// Windows. It is creack/pty, whose StartWithAttrs lets Setsid and Pdeathsig through.
//
// # What is alive does not reload
//
// The rule is stated in full on Reload, and the state model that makes it possible is the Drift
// type: an app's drift is not stored, it is derived from the declaration its last run was launched
// with and from the current one. A removed app survives as long as its process, restarts with what
// it had, and its supervisor withdraws itself as soon as it stops — "no longer declared and
// stopped" is therefore not a representable state.
//
// # Known limits
//
// Without a systemd box, a kill -9 of the tool leaves the intermediate runner orphaned: Pdeathsig
// only covers the processes one launches oneself, not those a runner detaches behind it. The log
// warns when the box is missing, and a normal stop takes everything down, box or not.
//
// Killing the systemd-run forwarder does not kill the session: detached — the usual case, the
// interface lending it its terminal — it only falls at its next written line; under the log stream,
// it falls with its pty.
//
// A facade that only has proxying and redirects has nothing to start: a start order on it does
// nothing, and it appears in no aggregate. It is not an unknown name for all that.
package core
