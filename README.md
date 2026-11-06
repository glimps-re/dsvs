# dsvs — a control panel for dev servers

[Français](README.fr.md)

dsvs runs the apps of a project — `ng serve`, `vite dev`, an API, any process — and serves them **behind a single port**, the way an ingress does in production: the `Host` of the request picks the facade, and its path picks the app, proxy target or redirect behind it. A command line, a TUI, and a panel behind a status bar icon steer them; each app has its own log; nothing starts on its own, you name what you want.

Several projects, or several worktrees of the same one, can run **at the same time on the same port**: a hub holds the port for all of them and hands each session the connections for the hosts it declares. The apps themselves open no port on the machine: they listen on virtual addresses, in a private network namespace that needs no privileges.

- An **app** is a process the configuration declares.
- A **facade** is a host and its routing table: it serves apps, proxy targets and redirects, and starts nothing itself.
- A **group** is a named set of apps, started and read in one go.
- A **session** runs the apps of one configuration. It lives without a terminal and survives the commands and interfaces that watch it.
- The **service** runs in the background: it puts the icon in the status bar and opens the panel.
- The **hub** holds the port. It belongs to no service: the first session that needs a port starts it, and it leaves with the last one.

Everything starts on demand, and says so.

## Requirements

- **Linux, with a `systemd --user` session.** The private network is made of network namespaces, and systemd runs the service and puts each session in a box. Without it, dsvs runs in a degraded mode and says so.
- **Node 22.18 or later (23.6 or later on the 23 line)** for a `config.dsvs.ts`: dsvs imports the TypeScript as is, through the type stripping node enables by default since those versions. A `dsvs.json` needs no node.
- **`curl`** to install, and **`~/.local/bin` in the `PATH`**.
- **An X11 status bar that hosts XEmbed icons**, for the icon. Without one, everything works from the terminal and the TUI, and `dsvs panel` opens the panel on an X display.

## Install

```sh
curl -fsSL https://github.com/glimps-re/dsvs/releases/latest/download/install.sh | sh
dsvs --version
```

The script downloads the binary of the latest release for this machine (amd64 or arm64), checks it against the `checksums.txt` of the release before it takes any place, and puts it in `~/.local/bin`. It then writes a `systemd --user` unit and starts the service: the icon is in the status bar when the prompt returns, and the last line says what happened. The unit starts the service again at each login. Since systemd, not a shell, starts it, the service takes the environment of your login shell: what it launches finds `node`, `pnpm` and the rest as a terminal would.

- `DSVS_VERSION=v0.1.0` installs that version instead of the latest.
- `DSVS_DIR=/some/bin` installs into another directory.
- `DSVS_RELEASES` points at a mirror laid out as the GitHub releases are.

`dsvs --version` says what is installed, `dsvs status` what runs. [CONTRIBUTING.md](CONTRIBUTING.md#releases) shows how to verify a release by hand, checksums and build provenance.

### The beta

The beta previews the next version. Its binary, `dsvs-beta`, installs **beside** `dsvs` and keeps to itself: its own sockets, unit, service, hub and remembered projects, and its own icon, with violet bars where the stable's are white. It reads its own configuration, `config.beta.dsvs.ts` or `dsvs.beta.json`, searched up from the current directory, and **never falls back** to `config.dsvs.ts`: a project runs on the beta once you copy its configuration, or name a file with `--config`. Give the copy another port and both binaries serve the project at once. The stable stays your fallback.

- From the panel of `dsvs`: **install the beta** shows up once a beta is published and none is installed. A second icon appears.
- From a terminal, even without a stable:

  ```sh
  curl -fsSL https://github.com/glimps-re/dsvs/releases/latest/download/install.sh | DSVS_CHANNEL=beta sh
  ```

`dsvs-beta` follows the pre-releases (`vX.Y.Z-beta.N`), and a stable release that overtakes the last beta. **uninstall the beta**, in its own panel, closes its sessions, removes its unit and its binary, and leaves `dsvs` as it was.

## Update

`dsvs update`, or **check for updates** then **update to vX.Y.Z** in the panel, which looks for a newer version each time it opens. A binary installed from a release downloads the new one over HTTPS and checks it against the checksums of its release before it replaces anything: `dsvs` follows the stable releases, `dsvs-beta` the pre-releases too. The old binary stays beside the new one as `.old`, and takes its place back if the new one does not start.

An update does not stop your apps. Each session hands over to the new binary without changing its PID: its systemd box sees nothing pass, and its apps remain its children. The update reports session by session, asking each one back which version it runs. The service and the gatekeeper of the private network hand over the same way. The hub is left alone: it takes the new binary when it leaves with its last session, and a session that loses it in the meantime starts another one without dropping anything.

A session running outside the systemd box keeps its binary until it stops: there, the kernel's parent-death signal is the only thing that keeps apps from being orphaned, and it would not survive the handover. A version that changes the protocol between sessions and their hub — rare, and the log says so — refuses to mix: while a session of the old one runs, a new session stops on a message naming both versions. Closing the open sessions is enough.

## Build from source

With Go and a clone, the path of whoever works on dsvs:

```sh
git clone https://github.com/glimps-re/dsvs.git
cd dsvs && ./build.sh --install     # → ~/.local/bin/dsvs, version = git describe
```

The binary is named after the branch — `main` builds `dsvs`, `feat/x` builds `dsvs-x` — and everything that isolates it derives from that name: sockets, icon, hub, systemd unit and remembered projects. A branch build therefore runs beside your everyday `dsvs` without touching it, and its icon has violet bars. The port, though, is global: the second binary asking for it is refused, with the name, version and session of the one holding it. To serve the same projects from a second binary, give it a port shift when installing it — `DSVS_PORT_SHIFT=100 ./build.sh --install`, or `dsvs-x --install-unit --port-shift 100` — which its unit keeps across reinstalls and updates: its facades then answer on the declared port plus 100.

A binary built from a clone updates from that clone: `dsvs update` fetches, rebuilds, replaces and restarts, saying each step. A clone with uncommitted changes refuses, since what it would build has no version to tell it apart; so does a clone left on another branch, since `dsvs-next` does not install under the name `dsvs`. Updating from a clone builds and runs what upstream carries: **whoever can push to that branch runs code on your machine**. That is the design, and it is what `git pull && ./build.sh --install` does by hand.

[CONTRIBUTING.md](CONTRIBUTING.md) covers the tests, the git hooks and the commit conventions.

## Usage

The block below is the output of `dsvs help`, generated from the command table by `go generate ./cmd/dsvs`: do not edit it by hand.

<!-- dsvs help -->
```
dsvs — control panel for dev-servers

  dsvs [options] <command> [options] [arguments]

Commands:
  start [--all] <name...>        start the apps these names pull in
  stop [--all] <name...>         stop the apps these names pull in
  restart [--all] <name...>      restart the apps these names pull in
  reload                         make the session read its configuration again
  set <name=value...>            give parameters of the configuration a value, and replay it
  quit                           stop the apps of the session and close it
  state [--json]                 write what the session here is doing
  logs [-n <lines>] [name...]    write the end of the logs, or follow them with -f
  run [name...] [name=value...]  hold the session in this terminal, streaming the logs
  status [--json]                write what runs on this machine, and who holds the ports
  service [stop|restart]         start the background service and its icon, or stop it
  update                         take a newer version, the beta for dsvs-beta, and restart the service
  panel [--png <file>]           open the panel of the running sessions
  remember                       remember the project here, so that the panel lists it cold
  forget                         forget the project here, which the panel stops listing
  config [name=value...]         write the resolved configuration as JSON
  types                          write the package specifier of the TypeScript types matching this binary
  skill                          write a skill that teaches this tool to a coding agent
  help [command]                 write what a command does, or the list of them

A name is an app, a facade — which pulls in the apps of its routes — or a group declared in the
configuration. The session port is taken when it starts and released when it stops; every facade
answers on it, and a path leading to a stopped app answers 502.

dsvs needs a configuration: the one named by --config, or the first one found searching up from the
current directory. Everything it needs around it starts on demand and says so — the background
service and its icon, then the session, which lives without a terminal and survives the commands and
interfaces that watch it.

dsvs with no command starts what the configuration found here needs, writes the state of its session
and returns; --tui opens the interface on that session instead. A configuration runs only once, and
two sessions never fight over a host: the second one is refused, and says so.

A few flags are the binary talking to itself — --tray, --hub, --session, --install-unit,
--remove-unit, --version — and are not commands.

Options, before or after the command:
  --config <file>
    	path of the configuration file (default: the first config.dsvs.ts found searching up)
  --net <network>
    	apps network: "auto", "netns" to require it, "loopback" to do without (default: auto)
  --port <port>
    	session port, instead of the one in the configuration
  --port-shift <offset>
    	offset added to the port the configuration declares, so that a second binary can serve the same projects
  --tui
    	open the interface on the session, instead of returning
  --watch <path>
    	path whose save makes the configuration reload, on top of those it declares; repeatable
```
<!-- /dsvs help -->

`dsvs help <command>` details a command and its options; `?` lists the keys of the TUI. `dsvs skill > ~/.claude/skills/dsvs/SKILL.md` installs a skill that teaches dsvs to a coding agent, drawn from the same command table.

### The icon and the panel

The icon reads without opening anything: lit bars and a solid green dot when something runs and all is well, a spinning amber ring while an app starts, a red dot with a cross when an app fell. When nothing runs, the bars dim and the dot goes away. Its tooltip gives the binary and its version.

A click opens the panel. The projects you have opened stay listed from one login to the next, cold, each with three gestures: start or stop its apps, open its TUI or raise it, or forget it. Each app has a start/stop and a restart pictogram, each facade opens in the browser, and the parameters of a session are set right there. At the bottom, two distinct gestures: stop the apps of every project, or quit dsvs, icon included; and the update, and the beta row described above. `dsvs panel --png <file>` draws the panel into a PNG, without a display.

## Configuration

The project folder holds a `config.dsvs.ts` — code, that node evaluates once — or a `dsvs.json`, when it has no reason to require node. Both follow the same contract: the extension decides what reads the file, and nothing downstream knows which one it was. dsvs searches for it up from the current directory; `--config` names another one.

A minimal `dsvs.json`, one facade and one app:

```json
{
  "port": 4200,
  "ingress": [
    {
      "host": "shop.localhost",
      "routes": [
        { "path": "/", "app": "front" },
        { "path": "/api", "proxy": { "target": "https://staging.example.com", "changeOrigin": true } }
      ]
    }
  ],
  "apps": [
    { "name": "front", "command": ["npm", "run", "start", "--", "--port", "{port}"] }
  ]
}
```

`dsvs start front`, then open `http://shop.localhost:4200/`. Browsers resolve `*.localhost` on their own: no `/etc/hosts`, no sudo.

The same in TypeScript, in a `config.dsvs.ts`:

```ts
import type { Config } from "dsvs";

export default {
  port: 4200,
  ingress: [/* ... */],
  apps: [/* ... */],
} satisfies Config;
```

The file is code: it can read a manifest, query the workspace, or import a file of targets kept out of git. `DSVS_CONFIG` holds its absolute path while it is evaluated, enough to locate itself without guessing. `export default` can also be a function, `async` included.

Node strips the types without checking them: `satisfies Config` only protects you in the editor. dsvs therefore checks the shape of what the file builds, and refuses it like a syntax error — the previous configuration keeps running — for a key it does not read, suggesting the one you probably meant, as for a value of the wrong type, each by its path: `apps[1]: unknown key "evn" (did you mean "env"?)`, `port: a number, not "4380"`. An older dsvs says the same of a key it does not know yet.

### The TypeScript types

The `types/` package declares every field and documents it: it is the reference. It holds declarations only: the code of `form()` and its rules comes from the binary, which serves it under the name `"dsvs"` to the configuration and to what it imports. The package is therefore not needed for the configuration to run, and a project installs it as an optional dependency, pinned to the tag of the binary:

```sh
pnpm add --save-optional "dsvs@$(dsvs types)"
```

The specifier is a `git+ssh` URL on this repository, so it needs an SSH key that GitHub knows. Without one, pnpm installs everything else and exits with success; only the editor shows the missing types, by an unresolved import. What pnpm says depends on its version and on the lockfile: an info line, a warning that carries the git error, or nothing at all. pnpm 11 does not even write into `package.json` a dependency it could not resolve, so `pnpm add` then changes nothing: add the types from a machine that has the key. The project's CI installs with `pnpm install --frozen-lockfile --no-optional` and leaves `*.dsvs.ts` out of its build tsconfig, so as not to type an import that is absent.

### Reference

`Config`, the default export:

| Key | Type | |
|---|---|---|
| `port` | number | The only port opened on the machine; every facade answers on it. |
| `ingress` | `Ingress[]` | The facades, one per host. Several can lead to the same app. |
| `apps` | `App[]` | |
| `groups` | `{ name, members }[]` | Sets started by their name and read on one line, their logs merged. `members` are app names, each once; a group holds no facade and no group. |
| `subnet` | string | Range of the apps' virtual addresses. Default `10.77.0.0/24`; change it if it collides with a VPN. The first address belongs to the gatekeeper. |
| `watch` | string[] | Paths whose save reloads the configuration, on top of `--watch`. A directory watches its `*.dsvs.ts` files, a file itself. Default: the folder of the configuration. A path that does not exist is refused. |

`Ingress`, a facade:

| Key | Type | |
|---|---|---|
| `host` | string | The host naming the facade, `"shop.localhost"` for instance. Omitted, the facade answers whatever no other one claims. |
| `name` | string | Its name in the interfaces and the logs. Default: its host. Never the name of an app. |
| `routes` | `Route[]` | What each path becomes. The most specific one wins, whatever the order, and `"/"` takes the rest; without it, a path with no route is a 404. |

`Route`: a `path` — a prefix without trailing slash, so a `baseHref` is written as it is — and exactly one of:

| Key | Type | |
|---|---|---|
| `app` | string | The app that receives these paths. |
| `proxy` | `{ target, secure?, changeOrigin? }` | A remote target, written as an entry of Angular's `proxy.conf.json`, same words, same defaults. `target` is an http or https URL whose path prefixes the request's. `secure: false` accepts a certificate that cannot be verified. `changeOrigin: true` sends the target's host instead of the browser's, which any remote vhost wants. |
| `redirect` | string | A 302 to this path. It does not carry the rest of the requested path. |

`App`:

| Key | Type | |
|---|---|---|
| `name` | string | |
| `command` | string[] | Argv, run without a shell, in a terminal dsvs lends it. An app does not choose its address: dsvs reserves one and replaces `{port}` and `{ip}`, here as in `env`. `{port}` is required: without it, nothing would know where to reach the app. |
| `env` | `Record<string, string \| undefined>` | Variables set on top of the session's environment. An `undefined` value sets nothing. |
| `dir` | string | Working directory, absolute or relative to the configuration. Default: its folder. |
| `color` | string | Its color in the logs: `red`, `green`, `yellow`, `blue`, `magenta` or `cyan`. Default: taken from a palette, in order. |

### Parameters: a form

A configuration can declare **parameters**: a form that the build consumes. `form(model, schema)` declares the fields by their default — a boolean makes a flag, a number a number, a string a text, a list a choice with the default first, an object a group — and the schema sets their rules; `config(form, build)` builds the configuration from the resolved values, once the form is valid. The rules are those of Angular's Signal Forms — `hidden`, `disabled`, `readonly`, `required`, `min`, `max`, `minLength`, `maxLength`, `pattern`, `validate`, `validateTree`, `apply`, `applyWhen` — plus four: `choices` and `defaultValue` give the choices and default of the moment (a choice can be disabled with its reason, and stays in sight), `keep` keeps a chosen value for the project, and `network` keeps it for the network the machine goes out by. The schema also receives the **observed network**: the links that are up and the routes out.

```ts
import { choices, config, defaultValue, disabled, form, hidden, keep, max, min, network, validateTree } from "dsvs";
import servers from "./servers.dsvs.ts";

// The front of a shop: its API comes from the local server (the api app) or from a deployed one,
// staging — reachable from the company network — or demo, public.
const params = form(
  { api: ["local", "staging", "demo"], deployed: { version: ["v2", "v3"], traces: false }, slowApi: false, products: 20, locale: ["en", "fr"] },
  (p, observed) => {
    keep(p.locale);

    // The server to reach depends on the network: chosen once per network, it comes back with it.
    network(p.api);

    // staging is only reachable from the company network, through its tunnel, and so are its traces.
    const atWork = observed?.links.some((link) => link.name.startsWith("tun")) ?? false;
    choices(p.api, ["local", { value: "staging", disabled: !atWork && "staging is only reachable from the company network" }, "demo"]);
    defaultValue(p.api, atWork ? "staging" : "demo");
    hidden(p.deployed, { when: ({ valueOf }) => valueOf(p.api) === "local" });
    disabled(p.deployed.traces, { when: !atWork && "traces are only reachable from the company network" });

    // What the local server serves is only set when it is the one reached.
    hidden(p.slowApi, { when: ({ valueOf }) => valueOf(p.api) !== "local" });
    hidden(p.products, { when: ({ valueOf }) => valueOf(p.api) !== "local" });
    min(p.products, 0);
    max(p.products, 5000);

    validateTree(p, ({ valueOf }) =>
      valueOf(p.locale) === "fr" && valueOf(p.api) === "demo" ? { field: p.locale, message: "demo has no fr build" } : null,
    );
  },
);

export default config(params, ({ api, deployed, slowApi, products, locale }) => ({
  port: 4380,
  ingress: [
    {
      host: "shop.localhost",
      routes: [
        { path: "/", app: locale === "fr" ? "front-fr" : "front" },
        { path: "/admin", app: "admin" },
        { path: "/api", ...(api === "local" ? { app: "api" } : { proxy: { target: `${servers[api]}/${deployed.version}`, changeOrigin: true } }) },
      ],
    },
  ],
  apps: [
    // A hidden field is undefined here: products and slowApi only serve the local server, traces only a deployed one.
    { name: "api", command: ["node", "api.mjs", "--port={port}", ...(api === "local" ? [`--products=${products}`, ...(slowApi ? ["--slow"] : [])] : [])] },
    { name: "front", command: ["pnpm", "start", "--port={port}"], env: { API_TRACES: String(api !== "local" && deployed.traces) }, reload: { stdin: "r\n" } },
    /* front-fr, admin… */
  ],
}));
```

**Where a value comes from.** A value is the first intention that exists: the one just asked for, the one kept for the network if the field follows it, the session's — chosen, or brought back by the last network that had one — and the one kept for the project if it keeps it. It holds if it is among the choices of the moment and no rule disables it; otherwise the default answers — or the first enabled choice — and the intention comes back once it is possible again. On a network that kept nothing, the value in force stays, and what you choose there is kept for it. A kept value is anything but the gesture in progress; one that the rules of its field no longer take, such as a `max` lowered under it, **yields**: to the bound it crosses for `min` and `max`, to the default otherwise. The configuration applies, the intention stays in memory, and dsvs says so while it lasts — in the log, under the field in the panel and the TUI, in `dsvs state` and in the output of the command that replayed it: `products=20 no longer holds (at most 15): products=15 until it does`. A kept value makes the project listed, as `dsvs remember` does, and `dsvs forget` forgets it with the project.

**A gesture is a submission.** `dsvs set` refuses, with its reason, a hidden, disabled or read-only field, an unreadable value, one outside the choices or disabled, and one that would make the form invalid, and nothing changes. Two linked fields are asked together: `dsvs set api=staging deployed.version=v3`. The panel and the TUI fill in like a form, in any order: a choice that requires another field stays chosen, the missing field says what it waits for, and nothing applies before the gesture that completes it, which applies everything at once; only then are the choices kept. A form that the context makes invalid anyway — a default that does not hold, an error on the whole form — keeps the configuration in force, and `dsvs state` says so; at start, `dsvs run name=value` picks what makes it valid.

**Hidden fields.** A hidden field has no value for the build: it is `undefined` there, every field of a hidden group too, and the build must not use it — `api === "local" ? products : 0`. The rules always see the value it keeps, which it finds again when it reappears. The types stay those of the model, since the schema is fixed: `api === "stagin"`, `p.deployed.verison` or a choice outside the list are type errors, a hidden field read without a guard is not. dsvs catches that: when a field is hidden, it builds a second time with its kept value, and if the result differs, the configuration uses it anyway and dsvs says so like a value that yields — `slowApi, products are hidden, yet apps.api.command[3] depends on them` — while the kept configuration stays the one built without it.

Without a form, the default export stays an object, or a function that ignores its arguments. `export const params` is the former shape, and is refused.

### Any program is a configuration

Node is one implementation of a contract that any executable can fulfil, passed with `--config`: `.json` is a bare configuration, `.ts`, `.mts`, `.js` and `.mjs` go through node, any other file is run itself. It receives in `DSVS_INPUT` one line of JSON — the intentions layer by layer, as text, flags included, and `observed`, `null` when nothing observes the network:

```json
{"asked":{"deployed.version":"v3"},"network":{"api":"staging"},"session":{"slowApi":"true"},"kept":{"locale":"en"},"observed":{"links":[{"name":"tun0","type":"tuntap","addrs":["10.8.0.2/24"]}],"exits":[{"via":"198.51.100.1","dev":"eth0"}]}}
```

`DSVS_CONFIG` holds its path, and its working directory is the file's folder. It writes on stdout `{"form":{"fields":[…],"errors":[…],"notices":[…]},"config":{…}}`: each resolved field in order — `name`, `type` (`choice`, `flag`, `text` or `number`), `choices`, `disabledChoices` with each reason, `value`, `from` (`asked`, `network`, `session`, `kept` or `default`), its states `hidden`, `disabled` and its `reasons`, `readonly`, `keep`, `network`, `errors`, and `yielded`, the kept value that no longer holds with its layer and errors — then `notices`, what the form says without refusing anything, and the configuration those values build, `null` when the form has an error. What it writes on stderr goes to the session log. A non-zero exit, unreadable JSON or ten seconds without an answer are an error that changes nothing. A `dsvs.json` is a bare configuration, without a form.

### Saving is enough

Facades, routes, proxy targets and groups are read again without restarting anything. So is the port: the session moves to the new one and leaves the old one, or, if it is taken, refuses the configuration and says who holds it. What is alive — the subnet, the processes — stays as it is, and the log says so. dsvs watches the configuration file itself, whatever its name, the paths it declares (`watch`), those of the command line (`--watch`), and otherwise its folder: an import that lives elsewhere must be declared, or saving it reloads nothing.

The apps follow the declaration, and no process is cut from under whoever uses it. An app declared during a session appears, ready to start. An app whose command changed is marked `↻ restart pending`: it runs with the previous one, and its restart takes the new one. An app removed from the configuration is marked `⊘ gone from the configuration`: it runs as long as you let it, restarts with what it had, and leaves the list once stopped. Its log can be read until the session ends: `dsvs logs worker` shows it, ending with its departure. Putting it back makes it an ordinary app again, to restart, which takes up that log.

## What runs during a session

By default the apps open **no port** on the machine: they live on virtual addresses in an unprivileged network namespace, whose traffic and DNS go out through the machine's own stack, VPN and system proxy included. `--net loopback` does without it, `--net netns` requires it instead of falling back silently. The session runs again inside a transient systemd unit, its box: a hard stop then leaves no process behind, not even what a runner detaches. Without a systemd session it runs without the box, says so, and that guarantee is gone.

`dsvs config` writes the configuration as dsvs sees it — defaults applied, routes ordered — in the JSON format it reads back. A configuration with a form comes out built from what a session opened here would have, plus the values asked on top, without its form: `dsvs config api=staging` is the snapshot of the shop on staging. `dsvs config --session` writes the one in force in the running session, and `--params --session` says where each value comes from.

## How it is built

Each package carries its decisions, what was tried and set aside, and its known limits in its package comment: `go doc ./internal/hub`, `go doc ./internal/core`, `go doc ./cmd/dsvs`, and so on. The seams between them:

- `config` loads and validates, and nothing downstream checks again; `core` supervises; `gateway` serves; `hub` holds the port and dispatches it; `fdpass` carries file descriptors, through a socket as across an exec, and frames the lines of the protocol; `ui/*` consume snapshots and an event stream that can overflow without losing a wake-up — log lines, though, can drop, and the stream says how many.
- `core.Network` says where the apps listen, `core.Front` who holds the port: two questions, two interfaces. `core.Gateway` and `core.Network` are the points of replacement — a driven nginx, loopback or netns — and a web interface would be one more consumer of the core.

## Contributing

Issues and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) explains how to build and test, the commit conventions and how releases are made; [SECURITY.md](SECURITY.md) how to report a vulnerability.

## License

dsvs is released under the [MIT License](LICENSE).
### Any program is a configuration

Node is one implementation of a contract that any executable can fulfil, passed with `--config`: `.json` is a bare configuration, `.ts`, `.mts`, `.js` and `.mjs` go through node, any other file is run itself. It receives in `DSVS_INPUT` one line of JSON — the intentions layer by layer, as text, flags included, and `observed`, `null` when nothing observes the network:

```json
{"asked":{"deployed.version":"v3"},"network":{"api":"staging"},"session":{"slowApi":"true"},"kept":{"locale":"en"},"observed":{"links":[{"name":"tun0","type":"tuntap","addrs":["10.8.0.2/24"]}],"exits":[{"via":"198.51.100.1","dev":"eth0"}]}}
```

`DSVS_CONFIG` holds its path, and its working directory is the file's folder. It writes on stdout `{"form":{"fields":[…],"errors":[…],"notices":[…]},"config":{…}}`: each resolved field in order — `name`, `type` (`choice`, `flag`, `text` or `number`), `choices`, `disabledChoices` with each reason, `value`, `from` (`asked`, `network`, `session`, `kept` or `default`), its states `hidden`, `disabled` and its `reasons`, `readonly`, `keep`, `network`, `errors`, and `yielded`, the kept value that no longer holds with its layer and errors — then `notices`, what the form says without refusing anything, and the configuration those values build, `null` when the form has an error. What it writes on stderr goes to the session log. A non-zero exit, unreadable JSON or ten seconds without an answer are an error that changes nothing. A `dsvs.json` is a bare configuration, without a form.

### Saving is enough

Facades, routes, proxy targets and groups are read again without restarting anything. What is alive — the port, the subnet, the processes — stays as it is, and the log says so. dsvs watches the configuration file itself, whatever its name, the paths it declares (`watch`), those of the command line (`--watch`), and otherwise its folder: an import that lives elsewhere must be declared, or saving it reloads nothing.

The apps follow the declaration, and no process is cut from under whoever uses it. An app declared during a session appears, ready to start. An app whose command changed is marked `↻ restart pending`: it runs with the previous one, and its restart takes the new one. An app removed from the configuration is marked `⊘ gone from the configuration`: it runs as long as you let it, restarts with what it had, and leaves the list once stopped. Putting it back makes it an ordinary app again, to restart.

## What runs during a session

By default the apps open **no port** on the machine: they live on virtual addresses in an unprivileged network namespace, whose traffic and DNS go out through the machine's own stack, VPN and system proxy included. `--net loopback` does without it, `--net netns` requires it instead of falling back silently. The session runs again inside a transient systemd unit, its box: a hard stop then leaves no process behind, not even what a runner detaches. Without a systemd session it runs without the box, says so, and that guarantee is gone.

`dsvs config` writes the configuration as dsvs sees it — defaults applied, routes ordered — in the JSON format it reads back. A configuration with a form comes out built from what a session opened here would have, plus the values asked on top, without its form: `dsvs config api=staging` is the snapshot of the shop on staging. `dsvs config --session` writes the one in force in the running session, and `--params --session` says where each value comes from.

## How it is built

Each package carries its decisions, what was tried and set aside, and its known limits in its package comment: `go doc ./internal/hub`, `go doc ./internal/core`, `go doc ./cmd/dsvs`, and so on. The seams between them:

- `config` loads and validates, and nothing downstream checks again; `core` supervises; `gateway` serves; `hub` holds the port and dispatches it; `fdpass` carries file descriptors, through a socket as across an exec, and frames the lines of the protocol; `ui/*` consume snapshots and an event stream that can overflow without losing a wake-up — log lines, though, can drop, and the stream says how many.
- `core.Network` says where the apps listen, `core.Front` who holds the port: two questions, two interfaces. `core.Gateway` and `core.Network` are the points of replacement — a driven nginx, loopback or netns — and a web interface would be one more consumer of the core.

## Contributing

Issues and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) explains how to build and test, the commit conventions and how releases are made; [SECURITY.md](SECURITY.md) how to report a vulnerability.

## License

dsvs is released under the [MIT License](LICENSE).
