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
// # Ruled out of the interface, and why
//
// Each was shipped then removed, or tried then abandoned, and each time a special case or one less
// level of hierarchy was enough.
//
// Choosing a proxy's target from the TUI: the real need is a pair of backends on two facades, which is
// a constant in the configuration. Removing it deleted the tool's only disk write and all the state
// that outlived a session.
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
