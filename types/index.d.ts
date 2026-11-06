// Shape of what config.dsvs.ts exports. The file is code, run by node, which strips its types: it can
// derive its apps from a helper, the workspace or the environment, only the result counts.
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
  /** The path to redirect to, with a 302. It does not carry over the rest of the requested path. */
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
   * required somewhere — without it, nothing would know where to reach the app.
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
  /** Color of the app in the logs. Default: taken from a palette, in order. */
  color?: Color;
}

export type Color = "red" | "green" | "yellow" | "blue" | "magenta" | "cyan";
