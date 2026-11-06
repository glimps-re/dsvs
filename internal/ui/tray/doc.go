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
// # Ruled out by measurement, not to retry
//
// The D-Bus/SNI tray, for lack of a watcher. Fyne and gio, which need cgo and GL under llvmpipe. A web
// panel, which undoes the integration and means writing two renderings.
package tray
