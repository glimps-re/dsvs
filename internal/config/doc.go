// Package config loads the declarative configuration, written in TypeScript and evaluated by node,
// which strips its types.
//
// # Parameters are a form, which the configuration consumes
//
// config.dsvs.ts is a module that node evaluates, dsvs.json the same contract written by hand or
// produced by anything. What makes it reread is not an event but a list of paths, the one Watched
// returns. An import that lives elsewhere — the uncommitted targets file left in the original
// repository — is therefore watched where it is, and declares itself for that.
//
// A module declares its parameters with form(model, schema), imported from "dsvs", and exports
// config(form, build). The model gives the fields, their type and their default; the schema, their
// rules: presence, disabling, read-only, current choices and default, validation, and what is kept
// (keep, for the project; network, for the observed network). The code comes from the binary: the
// shim installs a hook that serves "dsvs" from the embedded engine (form.js), whether a package is
// installed or not, and the types package carries only its declarations.
//
// Four owners, and none overflows onto another. The human owns intentions, sparse values as text:
// those of a gesture (Asked), of the session, and those the project keeps for itself and for each
// network; a default is never one of them. The machine owns the observation. The form owns the
// resolved state of each field, computed on every evaluation and never stored here. The configuration
// is built from the resolved values alone, and only when the form is valid: Load returns nil
// otherwise, and the form says why.
//
// A field's value is the first intention that exists, in this order: asked, kept for the network if
// the field follows it, chosen in the session, kept for the project if it keeps it. It holds if it
// parses in the field's type and is among its current choices; otherwise it is the current default.
// The cascade is the engine's, because keep and network are rules: Go only lays the layers into the
// input. A stale intention thus falls back to the default without an error, and comes back when the
// file makes it possible again. A kept intention — anything that is not the gesture in progress —
// that its field's rules refuse yields likewise, to the bound it exceeds for min and max, to the
// default otherwise, and the field says so (Field.Yielded): a file that changes does not block a
// session over a value that was set in it, and does not change it silently. The engine replays the
// evaluation until nothing yields any more. What stays invalid — a default that does not hold, an
// error of the whole form — is the file's fault. No rule writes: the graph has no effect, a
// circular dependency is an error that names its fields, and one evaluation by replay is enough.
//
// Accept is the only guard of writes: a value a field does not take, or that would leave the form
// invalid, changes nothing. Keeping and Following say, on the form being applied, what to keep and
// where. A gesture never yields: memory yields to it, when the gesture contradicts it.
//
// # What replays the configuration is a program
//
// The contract is that of a program, and node is one implementation of it. It receives in
// DSVS_INPUT a line of JSON — the layers of intentions (asked, network, session, kept), as text,
// flags included, and what the machine shows or null — and in DSVS_CONFIG the path of the
// configuration; stdin is empty and the working directory is the file's. It writes on stdout
// {"form": …, "config": …}: every resolved field, in the model's order, and the Config those values
// build, or null when the form has an error. What goes to stderr lands in the session's log; a
// non-zero exit, unreadable JSON or ten seconds without an answer are an error that changes nothing.
// The extension decides who runs it: .json is a bare Config, without a form, .ts, .mts, .js and .mjs
// go through node, any other file is executed itself. A module without config() exports a
// configuration, or a function called without a value, and its form is empty.
//
// What dsvs config writes is the snapshot of what a session opened here would build: the
// configuration alone, readable again as a dsvs.json.
//
// # DSVS_CONFIG is the only anchor
//
// The tool knows nothing of worktrees, monorepos or runners. It sets DSVS_CONFIG to the absolute path
// of the configuration before evaluating it, and that is all a script needs to locate itself: read a
// .git, suffix its hosts with the worktree's name, fall back on an untracked file left in the
// original repository. Those adapters live with whoever consumes them, so there is nothing here to
// evolve when a workspace changes shape.
//
// # The beta's own file
//
// dsvs-beta looks for config.beta.dsvs.ts, or dsvs.beta.json, and never for the stable's: a file
// written for the stable runs on the beta only once someone has decided so, by copying it or by
// naming it with --config. The beta and the stable can then serve the same project on two ports
// without a shift to set. Without its file, the beta says so instead of falling back.
//
// # Ruled out, and why
//
// An embedded scripting language (Lua, Starlark, a JS in Go) instead of node: measured on go1.26,
// Starlark adds 1.8 MB to the binary and goja 11.5 MB, and neither runs the existing TypeScript. We
// would lose the types without gaining autonomy, which only a static format truly gives — that is
// what dsvs.json is there for.
//
// Values on stdin or in argv, a program that returns only the Config while Go resolves the defaults,
// two evaluations per replay (the declaration, then the build): the environment already carries
// DSVS_CONFIG and reads from a shell with nothing to do, the default rule would be written twice, and
// every home-made program would have two modes.
//
// Values written in the configuration, or a values file it would read: that is state that outlives
// the session, and that is what the configuration does not have to carry. What outlives it is kept
// beside it, for the project, where the form says.
//
// A static export of the parameters (export const params), beside config(): two ways of saying the
// same thing, forever. The shim refuses the old one and says what replaces it.
//
// Open metadata, multiple selection, an array of objects, the kind of an error, touched and debounce
// from Signal Forms: the three interfaces are dsvs's own and would not read an invented key, a value
// stays a word on the command line and in memory, a rule writes its message, and an error comes from
// an evaluation, never from a keystroke.
//
// Discovering the apps by querying the workspace, with nx show: it worked, but a declarative file
// breaks less often and the query cost about six seconds at startup (measured).
//
// Reloading on the R key alone, without watching the file: saving is a discreet gesture, and a
// half-written configuration changes nothing and says so. Both paths therefore coexist.
//
// # The vocabulary
//
// What the configuration declares is an app, never a service: the word service keeps its systemd
// meaning, which nothing disputes, and the service is the background process.
package config
