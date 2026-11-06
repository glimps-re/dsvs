// Package tui is the interactive control panel: a list of apps on the left, the log of what is being
// watched on the right. It decides nothing about the state of the processes; it consumes the core's.
//
// # The mouse
//
// The mouse tracking needed for the wheel to move one notch takes the terminal's native selection
// away, so the TUI provides its own. It is virtualized — a selection lives in log coordinates, not
// screen coordinates, it follows scrolling, and a line wrapped over three screen lines stays one line
// that copying does not break. Clicking selects or gives focus; actions stay keyboard shortcuts.
//
// # Parameters
//
// The ones the form shows are items of the list, under the session, in the model's order: the name
// on the left, the value on the right, and each group under its dimmed title; a hidden field is not
// there, and a group whose fields are all hidden goes with its title, even when the configuration
// does not change. ⏎ or space moves to the next choice by replaying the configuration, skipping a
// disabled choice, and the displayed log does not change — a parameter is set, it has no log. ⏎ on
// a text or a number opens a one-line input in place of its value: ⏎ sends it, esc drops it, and
// nothing goes out before. A disabled or read-only field is dimmed and does not move. Under the
// cursor, a parameter states at the top of the log its reasons, its errors and the form's, and the
// refusal of the last gesture made on it; these lines cover the oldest lines of the log rather than
// reducing its height, which is the terminal size the apps receive. There is no menu: two or three
// choices fit in one gesture.
//
// A configuration the session does not apply — a save broke it, or it fails on today's network — puts
// a cross before the session's port, and its card says why, until the file applies again.
//
// # Ruled out of the interface, and why
//
// Each was shipped then removed, or tried then abandoned, and each time a special case or one less
// level of hierarchy was enough.
//
// Choosing a proxy's target from the TUI: the real need is a pair of backends on two facades, which is
// a constant in the configuration. Removing it deleted a state that outlived the session without
// knowing anything about the network; what carries over from one session to the next today is a
// parameter, kept where its form says.
//
// A facade as a group member: a group is a handle on what starts, and a facade does not start. It
// stays a valid name on the command line.
//
// A state of its own for facades, a hard-wired « all » item, selectable routes, a separate network
// log, two logs side by side, tabs.
//
// Giving the port back when nothing runs anymore: to free the port, you quit, and keeping it alive
// took a whole state machine.
//
// # Out of reach
//
// Raising the TUI's window on screen: a terminal tab is not an X window, and the window manager here
// does not advertise a way to activate a window. What the panel can promise is to open a new terminal,
// and otherwise to ask for attention.
package tui
