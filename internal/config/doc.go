// Package config loads the declarative configuration, written in TypeScript and evaluated by node,
// which strips its types.
//
// # The configuration is a value, not a living program
//
// config.dsvs.ts is a module that node evaluates once, dsvs.json the same contract written by hand or
// produced by anything. What makes it reread is not an event but a list of paths, the one Watched
// returns. An import that lives elsewhere — the uncommitted targets file left in the original
// repository — is therefore watched where it is, and declares itself for that.
//
// Changing backend means editing the file: no selector, no state kept elsewhere, hence no
// reconciliation, no dead target, no two sessions stepping on each other. Targets are not committed —
// the script imports a gitignored file, which is scripting and not a convention of the tool, which
// knows only the value it receives.
//
// # DSVS_CONFIG is the only anchor
//
// The tool knows nothing of worktrees, monorepos or runners. It sets DSVS_CONFIG to the absolute path
// of the configuration before evaluating it, and that is all a script needs to locate itself: read a
// .git, suffix its hosts with the worktree's name, fall back on an untracked file left in the
// original repository. Those adapters live with whoever consumes them, so there is nothing here to
// evolve when a workspace changes shape.
//
// # Ruled out, and why
//
// An embedded scripting language (Lua, Starlark, a JS in Go) instead of node: measured on go1.26,
// Starlark adds 1.8 MB to the binary and goja 11.5 MB, and neither runs the existing TypeScript. We
// would lose the types without gaining autonomy, which only a static format truly gives — that is
// what dsvs.json is there for.
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
