// The form engine of dsvs. The shim serves it under the name "dsvs" to the configuration and to everything
// it imports, from the binary: nothing needs installing for it to run, and the types package carries only
// its declarations. form() declares the fields, config() the build; on each evaluation the schema declares
// its rules, then each state is computed on demand, once. No rule writes: the only possible flaw of the
// graph is a cycle, and it is named.

const pathName = Symbol("dsvs.path");
const resolving = Symbol.for("dsvs.resolve");
const forms = new WeakSet();
const onOneField = new Set(["required", "min", "max", "minLength", "maxLength", "pattern", "choices", "default", "validate"]);

let declaring = null;
let rulesMade = 0;

export function form(model, schema = () => {}) {
  const built = { leaves: leavesOf(model, ""), schema };
  forms.add(built);
  return built;
}

export function config(built, build) {
  if (!forms.has(built)) throw new TypeError("config() takes the form that form() returns");
  return { [resolving]: (input) => resolve(built, build, input) };
}

export const schema = (logic) => logic;

export function apply(path, logic) {
  nameOf("apply", path);
  logic(path, declaring.observed);
}

export function applyWhen(path, when, logic) {
  const name = nameOf("applyWhen", path);
  const outer = declaring.guards;
  declaring.guards = [...outer, { id: rulesMade++, name, when }];
  try {
    logic(path, declaring.observed);
  } finally {
    declaring.guards = outer;
  }
}

export const applyWhenValue = (path, predicate, logic) => applyWhen(path, ({ value }) => predicate(value()), logic);

export const hidden = (path, { when = true } = {}) => declare("hidden", path, { when });
export const disabled = (path, { when = true } = {}) => declare("disabled", path, { when });
export const readonly = (path, { when = true } = {}) => declare("readonly", path, { when });
export const keep = (path, { when = true } = {}) => declare("keep", path, { when });
export const network = (path, { when = true } = {}) => declare("network", path, { when });
export const required = (path, { when = true, message } = {}) => declare("required", path, { when, message });
export const min = (path, bound, { message } = {}) => declare("min", path, { bound, message });
export const max = (path, bound, { message } = {}) => declare("max", path, { bound, message });
export const minLength = (path, bound, { message } = {}) => declare("minLength", path, { bound, message });
export const maxLength = (path, bound, { message } = {}) => declare("maxLength", path, { bound, message });
export const pattern = (path, bound, { message } = {}) => declare("pattern", path, { bound, message });
export const choices = (path, list) => declare("choices", path, { list });
export const defaultValue = (path, value) => declare("default", path, { value });
export const validate = (path, logic) => declare("validate", path, { logic });
export const validateTree = (path, logic) => declare("tree", path, { logic });

// The built-in validators, and the message they write when the rule gives none.
const checks = {
  required: (value) => (value === "" || value === false) && "required",
  min: (value, bound) => value < bound && `at least ${bound}`,
  max: (value, bound) => value > bound && `at most ${bound}`,
  minLength: (value, bound) => value.length < bound && `at least ${bound} characters`,
  maxLength: (value, bound) => value.length > bound && `at most ${bound} characters`,
  pattern: (value, bound) => !new RegExp(bound.source, bound.flags.replace(/[gy]/g, "")).test(value) && `does not match ${bound}`,
};

// A retained intent — anything but the gesture in progress — that the rules of its field no longer
// accept does not invalidate the form: it yields, to the bound it crosses for min and max, to the default
// otherwise, and the field says so. Yielding changes other values: the evaluation replays until nothing
// yields any more, and each field yields at most twice, to the bound then to the default. A default or a
// gesture that does not hold remains an error.
async function resolve(built, build, input) {
  const yielded = new Map();
  for (;;) {
    const { fields, problems, valueAt } = await evaluate(built, input, yielded);
    let moved = false;
    for (const field of fields.filter((one) => one.errors.length > 0)) {
      const already = yielded.get(field.name);
      if (!already && retained.includes(field.from)) {
        yielded.set(field.name, { yielded: { value: field.value, from: field.from, errors: field.errors }, edge: field.edge });
        moved = true;
      } else if (already?.edge !== undefined) {
        already.edge = undefined;
        moved = true;
      }
    }
    if (moved) continue;
    const valid = problems.length === 0 && fields.every((field) => field.errors.length === 0);
    const said = fields.map(({ edge, ...field }) => compact({ ...field, yielded: yielded.get(field.name)?.yielded }));
    const config = valid ? await build(valueAt("", false), input.observed) : null;
    const hidden = fields.filter((field) => field.hidden).map((field) => field.name);
    const notices = valid && hidden.length > 0 ? [await hiddenUse(hidden, config, () => build(valueAt(""), input.observed))].filter(Boolean) : [];
    return { form: { fields: said, errors: problems, notices }, config };
  }
}

// A hidden field does not exist for the build: it receives undefined, and only the rules see its stored
// value. hiddenUse builds a second time with it, and says what depends on it anyway — the path of the
// first difference — or that the build fails with it. The configuration used remains the one built
// without it.
async function hiddenUse(hidden, config, withHidden) {
  const named = hidden.length === 1 ? `${hidden[0]} is hidden` : `${hidden.join(", ")} are hidden`;
  const them = hidden.length === 1 ? "it" : "them";
  let other;
  try {
    other = await withHidden();
  } catch (error) {
    return `${named}, and the configuration fails with ${them}: ${String(error?.message ?? error).split("\n")[0]}`;
  }
  const at = difference(plain(config), plain(other), "");
  return at !== null && `${named}, yet ${at || "the configuration"} depends on ${them}`;
}

const retained = ["network", "session", "kept"];

// evaluate declares the rules and resolves each field once, those that yielded taking their bound or
// their default.
async function evaluate(built, input, yielded) {
  const names = new Set([""]);
  for (const { name } of built.leaves) {
    for (let at = name; at; at = at.slice(0, Math.max(0, at.lastIndexOf(".")))) names.add(at);
  }
  declaring = { rules: [], guards: [], names, leaves: built.leaves, observed: input.observed };
  let rules;
  try {
    const declared = built.schema(pathTo(""), input.observed);
    if (typeof declared?.then === "function") throw new TypeError("the schema of a form declares its rules at once: it is not async");
    rules = declaring.rules;
  } finally {
    declaring = null;
  }

  const memo = new Map();
  const busy = [];
  const once = (key, name, compute) => {
    if (memo.has(key)) return memo.get(key);
    const from = busy.findIndex((cell) => cell.key === key);
    if (from >= 0) throw new Error(`cycle: ${chain([...busy.slice(from), { name }])}`);
    busy.push({ key, name });
    try {
      memo.set(key, compute());
    } finally {
      busy.pop();
    }
    return memo.get(key);
  };
  const leaf = (name) => built.leaves.find((one) => one.name === name);
  const covers = (rule, name) => rule.name === "" || rule.name === name || name.startsWith(rule.name + ".");
  const context = (name) => ({
    value: () => valueAt(name),
    valueOf: (path) => valueAt(known("valueOf", path, names)),
    stateOf: (path) => stateOf(known("stateOf", path, names)),
    pathKeys: () => (name ? name.split(".") : []),
  });
  const run = (logic, name) => {
    if (typeof logic !== "function") return logic;
    const said = logic(context(name));
    if (typeof said?.then === "function") throw new TypeError(`${name || "the form"}: only validate and validateTree may be async`);
    return said;
  };
  const said = (rule, key) => once(`${rule.id}:${key}`, rule.name, () => run(rule[key], rule.name));
  const active = (rule) => rule.guards.every((guard) => Boolean(said(guard, "when")));
  const matching = (kind, name, exactly) =>
    rules.filter((rule) => rule.kind === kind && (exactly ? rule.name === name : covers(rule, name)) && active(rule));
  const flag = (kind, name) => once(`${kind}:${name}`, name, () => matching(kind, name).some((rule) => Boolean(said(rule, "when"))));
  const reasons = (name) =>
    once(`disabled:${name}`, name, () =>
      matching("disabled", name)
        .map((rule) => said(rule, "when"))
        .filter(Boolean)
        .map((why) => (typeof why === "string" ? why : "")),
    );
  const stateOf = (name) => ({ hidden: flag("hidden", name), disabled: reasons(name).length > 0, readonly: flag("readonly", name) });
  const last = (kind, name) => matching(kind, name, true).at(-1);

  // An option is a value, and the reason that disables it: false when it can be chosen.
  const optionsOf = (name) =>
    once(`choices:${name}`, name, () => {
      const { type, spec } = leaf(name);
      const rule = last("choices", name);
      const listed = rule ? said(rule, "list") : type === "choice" ? spec : type === "flag" ? [spec, !spec] : null;
      if (listed?.length === 0) throw new Error(`${name}: its choices leave none`);
      return listed && listed.map(option);
    });
  const choicesOf = (name) => optionsOf(name)?.map((one) => one.value) ?? null;
  const enabledOf = (name) => {
    const enabled = optionsOf(name)?.filter((one) => one.off === false).map((one) => one.value) ?? null;
    if (enabled?.length === 0) throw new Error(`${name}: its choices are all disabled`);
    return enabled;
  };
  const fallbackOf = (name) =>
    once(`default:${name}`, name, () => {
      const { type, spec } = leaf(name);
      const rule = last("default", name);
      return String(rule ? said(rule, "value") : type === "choice" ? spec[0] : spec);
    });
  // The cascade: the first intent that exists, among those the rules of this field let through. A
  // default is never an intent.
  const intentOf = (name) => {
    if (Object.hasOwn(input.asked, name)) return [input.asked[name], "asked"];
    if (yielded.has(name)) return [yielded.get(name).edge, "default"];
    if (Object.hasOwn(input.network, name) && flag("network", name)) return [input.network[name], "network"];
    if (Object.hasOwn(input.session, name)) return [input.session[name], "session"];
    if (Object.hasOwn(input.kept, name) && flag("keep", name)) return [input.kept[name], "kept"];
    return [undefined, "default"];
  };
  const resolved = (name) =>
    once(`value:${name}`, name, () => {
      const enabled = enabledOf(name);
      const fallback = fallbackOf(name);
      const [intent, from] = intentOf(name);
      if (intent !== undefined && reads(leaf(name).type, intent) && (!enabled || enabled.includes(intent))) return [intent, from];
      return [!enabled || enabled.includes(fallback) ? fallback : enabled[0], "default"];
    });
  const typed = (name) => {
    const [text] = resolved(name);
    const { type } = leaf(name);
    return type === "flag" ? text === "true" : type === "number" ? Number(text) : text;
  };
  const valueAt = (name, hiddenToo = true) => {
    if (leaf(name)) return typed(name);
    const values = {};
    for (const one of built.leaves.filter((one) => covers({ name }, one.name))) {
      const keys = one.name.slice(name ? name.length + 1 : 0).split(".");
      let at = values;
      for (const key of keys.slice(0, -1)) at = at[key] ??= {};
      at[keys.at(-1)] = hiddenToo || !flag("hidden", one.name) ? typed(one.name) : undefined;
    }
    return values;
  };
  const bound = (kind, name) => {
    const rule = last(kind, name);
    return rule && said(rule, "bound");
  };

  const fields = [];
  for (const { name, type } of built.leaves) {
    const [value, from] = resolved(name);
    const { hidden, disabled, readonly } = stateOf(name);
    const requiring = matching("required", name, true).find((rule) => Boolean(said(rule, "when")));
    const errors = [];
    let edge;
    // A field nobody can set is not validated: nobody could fix its error.
    if (!hidden && !disabled && !readonly) {
      const current = typed(name);
      if (requiring && checks.required(current)) errors.push(requiring.message ?? "required");
      for (const kind of ["min", "max", "minLength", "maxLength", "pattern"]) {
        const rule = last(kind, name);
        const failed = rule && checks[kind](current, bound(kind, name));
        if (failed) errors.push(rule.message ?? failed);
        if (failed && (kind === "min" || kind === "max")) edge ??= String(bound(kind, name));
      }
      for (const rule of matching("validate", name, true)) {
        const message = await rule.logic(context(name));
        if (message) errors.push(message);
      }
    }
    fields.push({
      name, type, choices: choicesOf(name) ?? undefined, disabledChoices: disabledOf(optionsOf(name)), value, from, hidden, disabled, reasons: reasons(name).filter(Boolean), readonly,
      required: Boolean(requiring), min: bound("min", name), max: bound("max", name), minLength: bound("minLength", name), maxLength: bound("maxLength", name),
      pattern: bound("pattern", name)?.source, keep: flag("keep", name), network: flag("network", name), errors, edge,
    });
  }
  // A tree error attached to a field nobody can set is dropped, as in Angular: no gesture could fix it.
  // Without a field, it belongs to the form.
  const problems = [];
  for (const rule of rules.filter((rule) => rule.kind === "tree" && active(rule) && !stateOf(rule.name).hidden)) {
    for (const one of [await rule.logic(context(rule.name))].flat()) {
      if (!one) continue;
      const target = one.field && fields.find((field) => field.name === one.field[pathName]);
      if (!target) problems.push(typeof one === "string" ? one : one.message);
      else if (!target.hidden && !target.disabled && !target.readonly) target.errors.push(one.message);
    }
  }
  return { fields, problems, valueAt };
}

// plain returns a configuration as it travels: as JSON, a RegExp by its source and its flags.
function plain(config) {
  return JSON.parse(JSON.stringify(config ?? null, (_, value) => (value instanceof RegExp ? { regexp: value.source, flags: value.flags } : value)));
}

// difference returns the path of the first difference between two JSON values, or null when there is
// none. A list item that has a name is designated by it: apps.api rather than apps[1].
function difference(a, b, at) {
  if (JSON.stringify(a) === JSON.stringify(b)) return null;
  if (Array.isArray(a) && Array.isArray(b)) {
    for (let i = 0; i < Math.max(a.length, b.length); i++) {
      const item = a[i] ?? b[i];
      const found = difference(a[i], b[i], typeof item?.name === "string" ? `${at}.${item.name}` : `${at}[${i}]`);
      if (found !== null) return found;
    }
    return null;
  }
  if (a !== null && b !== null && typeof a === "object" && typeof b === "object" && !Array.isArray(a) && !Array.isArray(b)) {
    for (const key of new Set([...Object.keys(a), ...Object.keys(b)])) {
      const found = difference(a[key], b[key], at ? `${at}.${key}` : key);
      if (found !== null) return found;
    }
    return null;
  }
  return at;
}

// option reads an entry of choices: a bare value, or { value, disabled }, where disabled is true or says
// why.
function option(entry) {
  if (entry === null || typeof entry !== "object") return { value: String(entry), off: false };
  return { value: String(entry.value), off: entry.disabled ? (typeof entry.disabled === "string" ? entry.disabled : "") : false };
}

// disabledOf returns the disabled options, each with its reason, or nothing when all can be chosen.
function disabledOf(options) {
  const off = (options ?? []).filter((one) => one.off !== false);
  return off.length > 0 ? Object.fromEntries(off.map((one) => [one.value, one.off])) : undefined;
}

// compact keeps only what a field has to say: a false state or an empty list is not written.
function compact(field) {
  return Object.fromEntries(Object.entries(field).filter(([, value]) => value !== false && !(Array.isArray(value) && value.length === 0)));
}

function declare(kind, path, rule) {
  const name = nameOf(kind, path);
  if (onOneField.has(kind) && !declaring.leaves.some((one) => one.name === name)) {
    throw new TypeError(`${kind}(${name || "the form"}): the rule is on one field, not on a group`);
  }
  declaring.rules.push({ id: rulesMade++, kind, name, guards: declaring.guards, ...rule });
}

function nameOf(kind, path) {
  if (!declaring) throw new Error(`${kind}() is called outside the schema of a form`);
  return known(kind, path, declaring.names);
}

function known(kind, path, names) {
  const name = path?.[pathName];
  if (typeof name !== "string") throw new TypeError(`${kind}(): not a path of the form`);
  if (!names.has(name)) throw new TypeError(`${kind}(): no field named ${name}`);
  return name;
}

function pathTo(name) {
  return new Proxy({}, {
    get: (_, key) => (key === pathName ? name : typeof key === "symbol" ? undefined : pathTo(name ? `${name}.${key}` : key)),
  });
}

function leavesOf(model, prefix) {
  return Object.entries(model).flatMap(([key, spec]) => {
    const name = prefix + key;
    if (Array.isArray(spec) && spec.length > 0 && spec.every((choice) => typeof choice === "string")) return [{ name, type: "choice", spec }];
    if (typeof spec === "boolean") return [{ name, type: "flag", spec }];
    if (typeof spec === "number") return [{ name, type: "number", spec }];
    if (typeof spec === "string") return [{ name, type: "text", spec }];
    if (spec !== null && typeof spec === "object" && !Array.isArray(spec)) return leavesOf(spec, name + ".");
    throw new TypeError(`form(): ${name} is neither a flag, a number, a text, a group nor a list of texts to choose from`);
  });
}

// reads says whether a text reads as the type of the field: only a number has a grammar.
function reads(type, text) {
  return type !== "number" || /^-?\d+(\.\d+)?$/.test(text);
}

// chain writes a cycle as the fields it goes through, without repeating a field that two adjacent
// computations share.
function chain(cells) {
  return cells
    .map((cell) => cell.name || "the form")
    .filter((name, i, names) => i === 0 || name !== names[i - 1])
    .join(" → ");
}
