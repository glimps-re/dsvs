// Package panel is the panel: a window that shows the sessions and their apps, and acts on them. No
// logs — that is the TUI's job, and it is what keeps the panel the size of a panel. What it reads from a
// session is a state serialized as JSON; the TUI needs a whole terminal, which travels by file
// descriptors.
//
// Pure-Go X11, without cgo: no modern graphics library is usable without linking C, and the binary is
// static and stays so. So we draw an image and push it to the server in strips, since an X request is
// bounded. A single column, a fixed width, and a height decided by the number of apps: nobody has to
// know it in advance. When it exceeds the screen's, the running projects fold into a one-line summary,
// with only one left unfolded — the one where something fell, or the one picked — rather than
// scrolling: the panel has neither wheel nor scrollbar.
//
// # Where it opens
//
// Against the rectangle of the icon that asked for it, on the side opposite the screen edge that icon
// is closest to: the icon is in the status bar, so that edge is the bar's. That is all there is to know
// — under Qubes the bar lives in dom0, and the VM's X server publishes neither _NET_WORKAREA nor struts,
// so a placement computed from the work area would put the panel on top of a bar at the bottom or on a
// side.
//
// Or in a file: Render pushes the same drawing into a PNG instead of a window, without an X server, for
// whoever has no screen — an agent, a bug report, an SSH session.
//
// # The visual grammar
//
// A rule never carries text. A background says one thing only: this is interactive — hover in the
// panel, where the keyboard never comes, the cursor in the TUI. A glyph says a state, never a type
// — and the shape carries it, the color only repeats it, because a font can miss a glyph and a
// theme can swallow a hue.
//
// The header says which binary the panel comes from; a branch binary adds the amber square of its
// icon, and the working dsvs carries none.
//
// # The gestures
//
// Three per project, two global, and as many distinct intents: never a button that does two. The README
// lists them. With no session running, only quitting dsvs is left: there is nothing to stop, and
// quitting would otherwise be unreachable.
//
// The beta adds one, on a row of its own under the others: dsvs offers to install it once one is
// out and none is beside it, next to the amber square of the icon it will bring, and the beta
// offers to uninstall itself, as discreet as quitting. Only a published binary offers either: one
// built from a clone follows its clone. Like the update, the gesture shows its steps while it works
// and offers nothing to press again.
//
// An app's gestures are pictograms in two fixed slots, which never move under the pointer; the state
// column writes what the click would do. After an order, the app offers nothing until its session's
// next state, or a few seconds if the order causes none: the first slot changes meaning with the
// state, and a second click would do the opposite of what one expects.
//
// Nothing is started when the panel opens: opening a session does not hand back control with four dev
// servers and their ports.
//
// A configuration the session does not apply — a save broke it, or it fails on today's network — is
// said on one line under the project's title, until the file applies again.
//
// What the panel gives to read is never elided, except a configuration the session does not apply:
// a gesture's failure comes from elsewhere, has no promised length, and takes the lines it needs.
// Cut to the width, a message only says its beginning, and it is the end that explains; a tooltip
// would come too late from the X server of a VM. What names gets elided — a project, an app — and
// so does that one line, whose details are in the log and in dsvs state.
//
// A failure reads where the gesture took place, without exception. From the panel, the project stays
// shown with its reason and is retried with a click; in a terminal, the error goes to standard error
// with a non-zero exit code. A failure that reads nowhere is the one thing we do not allow ourselves.
package panel
