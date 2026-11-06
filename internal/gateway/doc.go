// Package gateway holds the session's port: it picks the facade by the request's Host, then the
// route by its path, like the ingress controller in production.
//
// Routes are declared on the facade, never on the app: several facades can serve the same app, at
// different paths, and it is the facade that knows which. Declaring routes on the app forced a
// choice between the two.
//
// FlushInterval is -1 on every proxy, and it is not a performance setting: without it, the HMR
// websocket and SSE streams die buffered. Checked end to end, HMR is proxied as a 101 on two
// facades at once.
//
// A proxy is kept per target, and rebuilt when the target changes run: an app that restarts
// elsewhere would otherwise leave warm connections to nowhere. These proxies are never swept —
// bounded by the number of targets ever seen, and accepted.
package gateway
