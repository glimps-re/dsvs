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
// A browser that opens the address of an app that is starting gets a waiting page, a 503 that says
// what the 502 would say and reloads itself when the address serves something else — the app ready,
// or down, and the 502 then says why. Any other client keeps the immediate 502: the page only makes
// sense to whoever runs its script. An app that listens elsewhere will not answer on its own, and
// gets no page. The script and its stream live under /__dsvs/, which the proxy serves on every
// address before any route: the stream tells facts — apps, where the facade's apps stand, and
// config, the generation of the applied configuration — and the script reloads the page when one of
// those it carries is no longer the one it was served with: a comparison rather than a signal, so
// that nothing is lost between the page and the opening of the stream.
//
// An app whose declaration says reload: "page" serves its documents with that script, which reloads
// them on each applied configuration. A document is the HTML response to a navigation: a fetch, a
// script or a fragment pass through untouched, and so does HMR. The script is appended after the
// last byte, which does not hold back a streamed document; the announced length goes away and the
// response goes out chunked. So that there is something to append to, the app receives the
// navigation without Accept-Encoding — a document it compresses anyway passes through untouched —
// and without conditions: a 304 would resurrect a copy carrying another generation, which would
// reload endlessly, so the page goes out no-store, without ETag. A CSP that refuses the script
// leaves the page intact, without reloading.
//
// A proxy is kept per target, and rebuilt when the target changes run: an app that restarts
// elsewhere would otherwise leave warm connections to nowhere. A remote target also rebuilds it
// when the machine changes network: the connection kept on the old one is dead without knowing it,
// and the first request on it waited tens of seconds for a response. These proxies are never swept
// — bounded by the number of targets ever seen, and accepted.
package gateway
