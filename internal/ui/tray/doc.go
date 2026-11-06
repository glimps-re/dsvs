// Package tray places dsvs's icon in the status bar, and opens the panel on click.
//
// XEmbed, in pure Go, with jezek/xgb and CGO_ENABLED=0 preserved. The environment dictates it: a Qubes
// VM, the qubes-gui window manager, and no StatusNotifierWatcher on D-Bus. Every modern tray library —
// SNI, libappindicator, hence cgo — is out, and the only tray available is XEmbed, whose selection
// qubes-gui holds before relaying the icon to dom0. That relay is what makes the protocol unforgiving:
// without all its properties, the icon is accepted on the VM side and never shown by dom0.
//
// # What the spike measured
//
// The click comes back down with root coordinates, hence with what it takes to open the panel under
// the icon without tracking anything. The tooltip, which the status bar takes from the WM_NAME of the
// icon's window, is the only place that holds the full name and the version; the bar tint tells the
// binaries apart at a glance.
//
// # What the icon says
//
// Three bars on a dark tile, drawn as an image at the size the bar imposes. The bar tint says which
// binary placed the icon — white for dsvs, violet for any other —, their luminance whether something
// runs, and a badge in the corner says how, as soon as an app runs: a full, still disc when all is well,
// a half-full ring that spins while an app starts, a crossed disc when an app has fallen. When nothing
// runs, no badge. We first wanted calm to have no badge, so that it would only mean "come and look"; in
// use, knowing without opening the panel that something runs proved more useful. Every state differs by
// its shape and luminance, never by tint alone, because dom0 tints the icon with the qube's color.
//
// The sessions are followed through what they push, and the hub is listed again every three seconds
// for new ones, which it does not announce. The icon repaints only when what they say together changes;
// the only timer is the quarter turn every 500 ms, and it exists only during "starting". The window's
// name stays the binary's label: dom0 was never seen relaying a live name change, so the state is not
// written there.
//
// # Ruled out by measurement, not to retry
//
// The D-Bus/SNI tray, for lack of a watcher. Fyne and gio, which need cgo and GL under llvmpipe. A web
// panel, which undoes the integration and means writing two renderings.
package tray
