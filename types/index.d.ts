// Shape of what config.dsvs.ts exports, and declarations of the form of its parameters. The file is
// code, run by node, which strips its types: it can derive its apps from a helper, the workspace or
// the environment, only the result counts. The code of `form()` and its rules comes from the binary,
// which serves it under the name "dsvs": this package carries only its types.
export interface Config {
  /** The only port open on the machine, like the ingress controller in production. */
  port: number;
  /** The facades, one per host. Several may lead to the same app. */
  ingress: Ingress[];
  apps: App[];
  /** The sets started and read in a single move. */
  groups?: Group[];
  /**
   * The range of the apps' virtual addresses, in the private network. Change it if it collides with
   * a VPN. Default: `10.77.0.0/24`. The first address stays with the gatekeeper.
   */
  subnet?: string;
  /**
   * The paths whose save makes the configuration reload, on top of those `--watch` adds. A
   * directory watches its `*.dsvs.ts` files, a file watches itself, whatever its name. Default: the
   * configuration's directory — to be declared, then, as soon as it imports a file that lives
   * elsewhere. A path that does not exist is refused at load.
   */
  watch?: string[];
}

/**
 * A named set, started by its name on the command line and read as one line in the interface: its
 * log merges those of its members.
 */
export interface Group {
  name: string;
  /**
   * App names, each once. A facade is not one: it does not start, it is served as long as the
   * session lives. A group does not contain a group.
   */
  members: string[];
}

export interface Ingress {
  /**
   * The host that designates this facade — `"admin.localhost"` for instance; browsers resolve
   * `*.localhost` to loopback on their own. Omitted, the facade answers for everything no other one
   * claims.
   */
  host?: string;
  /** Its name in the interface and in the logs. Default: its host. Never that of an app. */
  name?: string;
  /**
   * What each path becomes. The most specific wins, whatever the order of writing, and `"/"` takes
   * everything left — without it, a path without a route is a 404.
   */
  routes: Route[];
}

/** A route leads to an app, a proxy or a redirect: exactly one of the three. */
export interface Route {
  /** Path prefix, without a trailing slash — a `baseHref` is therefore written as is. */
  path: string;
  /** The name of the app that receives these paths. */
  app?: string;
  /** The remote target of these paths. This file is the only place where it changes. */
  proxy?: Target;
  /**
   * The path to redirect to, with a 302, followed by what follows `path` in the requested path and by
   * its query: with `{ path: "/", redirect: "/shop" }`, `/products/12?color=red` leads to
   * `/shop/products/12?color=red`, and `/` to `/shop`. Another route must take the target: this one
   * would send it back under itself.
   */
  redirect?: string;
}

/** A proxy target is written like an entry of `proxy.conf.json`: same words, same defaults. */
export interface Target {
  /** The backend URL, in http or https. Its path, if it has one, prefixes the request's. */
  target: string;
  /**
   * False accepts a certificate that cannot be verified, as development backends have.
   * Default: true.
   */
  secure?: boolean;
  /**
   * True makes the request carry the target's host instead of the browser's: what any remote vhost
   * wants. Default: false.
   */
  changeOrigin?: boolean;
}

export interface App {
  name: string;
  /**
   * Argv, run without a shell, in a terminal the tool lends it. An app does not choose its address:
   * the tool reserves one for it and replaces `{port}` and `{ip}`, here as in `env`. `{port}` is
   * required somewhere for an app a facade serves — without it, nothing would know where to reach
   * it; an app no route leads to does not need it (`tsc --watch`, a worker).
   */
  command: string[];
  /**
   * The variables set on top of the session's environment. An `undefined` value sets none: an
   * object built from a condition is written as is, and apps with different variables fit in the
   * same array.
   */
  env?: Record<string, string | undefined>;
  /** Working directory, absolute or relative to the configuration file. Default: its directory. */
  dir?: string;
  /** Color of the app in the logs. Default: drawn from its name in a palette, the same when other apps come or go. */
  color?: Color;
  /**
   * What says the app is ready, when its command does not. Without this key: an app a facade serves
   * is ready when a real request gets an answer, an app that receives `{port}` without a route when
   * its port opens, an app that receives none as soon as it is started. Once per start: a restart
   * goes back to "starting".
   */
  ready?: Ready;
  /**
   * What an applied configuration — a save, `dsvs set`, a network change — does so that what it
   * changes is seen. Without this key, nothing. `"page"`: the HTML pages the app serves, open in a
   * browser, reload; a route must lead to the app. `{ stdin }`: this text is typed into the terminal of
   * the ready app, as is — nothing is added, not even a newline — like `{ stdin: "r\n" }` for
   * `ng serve`, where it is the "force reload browser".
   */
  reload?: "page" | { stdin: string };
}

/**
 * Exactly one of the two forms. `http`: dsvs requests this path on `{ip}:{port}`, and the app is ready
 * at the first 2xx code; the command must therefore receive `{port}`. `log`: the app is ready when a
 * line of its output contains this text, or matches this expression — read by Go (RE2): the `i`, `m`
 * and `s` flags keep their meaning, a lookbehind or a backreference is refused with the configuration.
 */
export type Ready = { http: `/${string}`; log?: never } | { log: string | RegExp; http?: never };

export type Color = "red" | "green" | "yellow" | "blue" | "magenta" | "cyan";

/**
 * The model of a form: each field by its default, which also gives its type. A boolean makes a flag,
 * a number a number, a text a text, a list of texts a choice — the default first, and the list is the
 * universe of its values —, an object a group, whose name precedes those of its fields
 * (`api.deployment`). The order is the display order.
 */
export type Model = { readonly [name: string]: boolean | number | string | readonly [string, ...string[]] | Model };

/**
 * The values the build receives: a boolean per flag, a number per number, a text per text, one of the
 * values of each choice. A disabled field has its value too. A hidden field, though, is `undefined`
 * there — a hidden group, each of its fields —, which these types do not say: the schema is fixed, and
 * whoever reads a field a rule can hide guards it themselves.
 */
export type Values<M> = {
  -readonly [K in keyof M]: M[K] extends readonly string[]
    ? M[K][number]
    : M[K] extends boolean
      ? boolean
      : M[K] extends number
        ? number
        : M[K] extends string
          ? string
          : Values<M[K]>;
};

declare const valueOfPath: unique symbol;

/** A path in the form, typed by the value it designates: `p.api.deployment`. */
export type Path<V> = { readonly [valueOfPath]?: V } & (V extends string | number | boolean
  ? {}
  : { readonly [K in keyof V]-?: Path<V[K]> });

/** What a field is, for another rule. */
export interface FieldState {
  hidden: boolean;
  disabled: boolean;
  readonly: boolean;
}

/**
 * What a rule reads. It adapts to the values through `valueOf`, which is reactive, and to the network by
 * closing over `observed`, which the schema receives: an evaluation is a fresh process, whose context
 * is fixed.
 */
export interface Context<V> {
  /** The value of the field or group the rule is set on. */
  value(): V;
  valueOf<T>(path: Path<T>): T;
  stateOf(path: Path<unknown>): FieldState;
  /** The path the rule is set on: what tells a schema applied to two groups which one it is in. */
  pathKeys(): readonly string[];
}

/** A rule: a value, or a pure function of what it reads. No rule writes. */
export type Logic<V, R> = R | ((context: Context<V>) => R);

type Leaf = string | number | boolean;

declare const modelOfForm: unique symbol;

/** What `form()` returns, and `config()` consumes. */
export interface Form<M> {
  readonly [modelOfForm]: M;
}

/** What `config()` returns: the configuration's default export. */
export interface Configuration {
  readonly [modelOfForm]: "configuration";
}

/** A set of rules, applied to a path, to two groups for instance. */
export type Schema<V> = (path: Path<V>, observed: Observed | null) => void;

/** An error of `validateTree`: a message for the form, or attached to a field. */
export type TreeError = string | { field?: Path<unknown>; message: string } | null | undefined | false;

/**
 * Declares the fields and their rules. The schema is replayed at each evaluation, with what the machine
 * shows: it only declares. A move that would make the form invalid is refused. A kept value — by the
 * session, `keep` or `network` — that the rules of its field no longer accept yields, and dsvs says so:
 * to the bound it exceeds for `min` and `max`, to the default otherwise, and it comes back as soon as
 * it holds. An evaluation the form refuses anyway — a default that does not hold, an error of the whole
 * form — keeps the configuration in force.
 */
export declare function form<const M extends Model>(model: M, schema?: Schema<Values<M>>): Form<M>;

/**
 * Builds the configuration from the resolved values of the form, when it is valid: this is the default
 * export. `async` is allowed.
 */
export declare function config<M>(
  form: Form<M>,
  build: (values: Values<M>, observed: Observed | null) => Config | Promise<Config>,
): Configuration;

/** A schema to apply; the function itself, typed. */
export declare function schema<V>(schema: Schema<V>): Schema<V>;
export declare function apply<V>(path: Path<V>, schema: Schema<V>): void;
/** The rules of the schema only hold while the condition holds, read on the path. */
export declare function applyWhen<V>(path: Path<V>, when: (context: Context<V>) => boolean, schema: Schema<V>): void;
export declare function applyWhenValue<V>(path: Path<V>, predicate: (value: V) => boolean, schema: Schema<V>): void;

/**
 * The field, or everything the group contains, exists for no interface: neither shown, nor validated,
 * nor settable. Its value is kept, and comes back with it: the rules see it, the build does not, where
 * it is `undefined` (see `Values`). A configuration that uses it anyway is told so, by the path of what
 * depends on it: `discount is hidden, yet apps.api.env.DISCOUNT depends on it`.
 */
export declare function hidden<V>(path: Path<V>, options?: { when?: Logic<V, boolean> }): void;
/** Shown, inert, and not validated. A reason, if there is one, is written under the field and in the refusal. */
export declare function disabled<V>(path: Path<V>, options?: { when?: Logic<V, boolean | string> }): void;
/** Shown, not settable and not validated: an imposed value. */
export declare function readonly<V>(path: Path<V>, options?: { when?: Logic<V, boolean> }): void;
/**
 * The chosen value outlives the session, for this project on this machine, and the project is from then
 * on remembered, as by `dsvs remember`.
 */
export declare function keep<V>(path: Path<V>, options?: { when?: Logic<V, boolean> }): void;
/**
 * The chosen value is kept for the observed network — the preferred exit, `eth0 via 198.51.100.1` —
 * and comes back with it, a new session included. On a network that kept nothing, the value in force
 * stays; a session opening there has none, and takes what `keep` keeps, otherwise the default. With
 * `keep`, a known network answers first. As with `keep`, the project is from then on remembered.
 */
export declare function network<V>(path: Path<V>, options?: { when?: Logic<V, boolean> }): void;

/**
 * The current choices, in display order. On a text or a number, they restrict it. An option can be
 * disabled, with its reason: `{ value: "staging", disabled: !vpn && "staging needs the VPN" }`. It
 * stays in the list, greyed out with its reason, and is never taken: a move that chooses it is refused,
 * and a kept value it designates falls back to the default — or to the first enabled choice, when the
 * default is itself disabled —, then comes back when the option is enabled again.
 */
export declare function choices<V extends Leaf>(path: Path<V>, list: Logic<V, readonly Choice<NoInfer<V>>[]>): void;
/** An entry of `choices`: a value, or a value disabled when `disabled` is true or says why. */
export type Choice<V> = V | { value: V; disabled?: boolean | string };
/** The current default, as long as nobody has chosen; a choice made comes back as soon as it is possible. */
export declare function defaultValue<V extends Leaf>(path: Path<V>, value: Logic<V, NoInfer<V>>): void;

export declare function required(path: Path<string> | Path<boolean>, options?: { when?: Logic<string, boolean>; message?: string }): void;
export declare function min(path: Path<number>, bound: Logic<number, number>, options?: { message?: string }): void;
export declare function max(path: Path<number>, bound: Logic<number, number>, options?: { message?: string }): void;
export declare function minLength(path: Path<string>, bound: Logic<string, number>, options?: { message?: string }): void;
export declare function maxLength(path: Path<string>, bound: Logic<string, number>, options?: { message?: string }): void;
export declare function pattern(path: Path<string>, pattern: Logic<string, RegExp>, options?: { message?: string }): void;
/**
 * Returns a message when the value is wrong. May be `async`: the evaluation waits for it, within its
 * ten seconds, at each replay.
 */
export declare function validate<V extends Leaf>(
  path: Path<V>,
  logic: (context: Context<V>) => string | null | undefined | false | Promise<string | null | undefined | false>,
): void;
/** The rule of a group or of the whole form; may be `async`. */
export declare function validateTree<V>(
  path: Path<V>,
  logic: (context: Context<V>) => TreeError | readonly TreeError[] | Promise<TreeError | readonly TreeError[]>,
): void;

/**
 * What the machine shows, and nobody sets. null when nothing observes it: netlink failed, and the
 * session's log says why.
 */
export interface Observed {
  /** The interfaces that are up, with carrier, and have at least one global address, sorted by name. */
  links: Link[];
  /**
   * Where the traffic leaves the machine: its default routes, the preferred first (the lowest
   * metric), IPv4 before IPv6. Empty when nothing goes out.
   */
  exits: Exit[];
}

export interface Exit {
  /** The gateway: `"192.0.2.1"`. Empty for a route without a gateway, a tun's. */
  via: string;
  /** The interface traffic leaves through: `"eth0"`, `"tun0"`. */
  dev: string;
}

export interface Link {
  /** `"wlan0"`, `"tun0"`, `"wg0"`… */
  name: string;
  /** The type the kernel gives it: `"device"`, `"tuntap"`, `"wireguard"`, `"bridge"`… */
  type: string;
  /** Its global addresses, in CIDR: `"10.8.0.2/24"`. */
  addrs: string[];
}
