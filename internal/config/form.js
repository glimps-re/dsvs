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

async function resolve(built, build, input) {
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

  const choicesOf = (name) =>
    once(`choices:${name}`, name, () => {
      const { type, spec } = leaf(name);
      const rule = last("choices", name);
      const listed = rule ? said(rule, "list") : type === "choice" ? spec : type === "flag" ? [spec, !spec] : null;
      if (listed?.length === 0) throw new Error(`${name}: its choices leave none`);
      return listed && listed.map(String);
    });
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
    if (Object.hasOwn(input.network, name) && flag("network", name)) return [input.network[name], "network"];
    if (Object.hasOwn(input.session, name)) return [input.session[name], "session"];
    if (Object.hasOwn(input.kept, name) && flag("keep", name)) return [input.kept[name], "kept"];
    return [undefined, "default"];
  };
  const resolved = (name) =>
    once(`value:${name}`, name, () => {
      const listed = choicesOf(name);
      const fallback = fallbackOf(name);
      const [intent, from] = intentOf(name);
      if (intent !== undefined && reads(leaf(name).type, intent) && (!listed || listed.includes(intent))) return [intent, from];
      return [!listed || listed.includes(fallback) ? fallback : listed[0], "default"];
    });
  const typed = (name) => {
    const [text] = resolved(name);
    const { type } = leaf(name);
    return type === "flag" ? text === "true" : type === "number" ? Number(text) : text;
  };
  const valueAt = (name) => {
    if (leaf(name)) return typed(name);
    const values = {};
    for (const one of built.leaves.filter((one) => covers({ name }, one.name))) {
      const keys = one.name.slice(name ? name.length + 1 : 0).split(".");
      let at = values;
      for (const key of keys.slice(0, -1)) at = at[key] ??= {};
      at[keys.at(-1)] = typed(one.name);
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
    // A field nobody can set is not validated: nobody could fix its error.
    if (!hidden && !disabled && !readonly) {
      const current = typed(name);
      if (requiring && checks.required(current)) errors.push(requiring.message ?? "required");
      for (const kind of ["min", "max", "minLength", "maxLength", "pattern"]) {
        const rule = last(kind, name);
        const failed = rule && checks[kind](current, bound(kind, name));
        if (failed) errors.push(rule.message ?? failed);
      }
      for (const rule of matching("validate", name, true)) {
        const message = await rule.logic(context(name));
        if (message) errors.push(message);
      }
    }
    fields.push({
      name, type, choices: choicesOf(name) ?? undefined, value, from, hidden, disabled, reasons: reasons(name).filter(Boolean), readonly,
      required: Boolean(requiring), min: bound("min", name), max: bound("max", name), minLength: bound("minLength", name), maxLength: bound("maxLength", name),
      pattern: bound("pattern", name)?.source, keep: flag("keep", name), network: flag("network", name), errors,
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
  const valid = problems.length === 0 && fields.every((field) => field.errors.length === 0);
  return { form: { fields: fields.map(compact), errors: problems }, config: valid ? await build(valueAt(""), input.observed) : null };
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
