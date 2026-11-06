package config

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The engine tests go through node, like an evaluation: a model, a schema, an input, and what the form
// and the build make of them.

const rules = `import { apply, applyWhen, applyWhenValue, choices, config, defaultValue, disabled, form, hidden, keep, max, maxLength, min, minLength, network, pattern, readonly, required, schema, validate, validateTree } from "dsvs";`

// resolved evaluates this schema on this model. The build returns the values it received in the
// environment of its app, as JSON.
func resolved(t *testing.T, model, schema string, in Input) (Form, map[string]any, error) {
	t.Helper()
	body := rules + `
export default config(form(` + model + `, (p, observed) => {
` + schema + `
}), (values) => ({ ` + minimal + `, apps: [{ name: "a", command: ["srv", "{port}"], env: { VALUES: JSON.stringify(values) } }] }));`
	form, cfg, err := Load(write(t, body), nil, in)
	if err != nil || cfg == nil {
		return form, nil, err
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(cfg.Apps[0].Env["VALUES"]), &values); err != nil {
		t.Fatal(err)
	}
	return form, values, nil
}

func field(t *testing.T, form Form, name string) Field {
	t.Helper()
	found, declared := form.Field(name)
	if !declared {
		t.Fatalf("no field %s in %+v", name, form)
	}
	return found
}

// The cascade: asked, kept for the network if the field follows it, chosen in the session, kept for the
// project if the field keeps it, then the default. An intent that does not parse, or is outside the
// choices, falls back to the default.
func TestAValueIsTheFirstIntentThatHolds(t *testing.T) {
	t.Parallel()
	const model = `{ a: ["x", "y", "z", "w"], b: ["x", "y", "z", "w"], n: 3 }`
	const schema = `keep(p.a); network(p.a);`
	cases := []struct {
		name        string
		in          Input
		field       string
		value, from string
	}{
		{"nothing", Input{}, "a", "x", "default"},
		{"the project", Input{Kept: map[string]string{"a": "y"}}, "a", "y", "kept"},
		{"the session over the project", Input{Kept: map[string]string{"a": "y"}, Session: map[string]string{"a": "z"}}, "a", "z", "session"},
		{"the network over the session", Input{Session: map[string]string{"a": "z"}, Network: map[string]string{"a": "w"}}, "a", "w", "network"},
		{"the request over everything", Input{Network: map[string]string{"a": "w"}, Asked: map[string]string{"a": "y"}}, "a", "y", "asked"},
		{"without keep, the project does not answer", Input{Kept: map[string]string{"b": "y"}}, "b", "x", "default"},
		{"without network, the network does not answer", Input{Network: map[string]string{"b": "y"}, Session: map[string]string{"b": "z"}}, "b", "z", "session"},
		{"outside the choices", Input{Session: map[string]string{"a": "nope"}}, "a", "x", "default"},
		{"a number", Input{Session: map[string]string{"n": "250"}}, "n", "250", "session"},
		{"an unreadable number", Input{Session: map[string]string{"n": "abc"}}, "n", "3", "default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			form, _, err := resolved(t, model, schema, c.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := field(t, form, c.field); got.Value != c.value || got.From != c.from {
				t.Errorf("%s = %q from %q, want %q from %q", c.field, got.Value, got.From, c.value, c.from)
			}
		})
	}
}

// Choices that narrow make the value fall back to the current default, then to the first choice; the
// intent stays, and comes back with them. A dynamic default follows its source as long as nobody has
// chosen.
func TestChoicesAndDefaultsFollowWhatTheyRead(t *testing.T) {
	t.Parallel()
	const model = `{ link: ["tun", "none"], via: ["office", "vpn", "public"], mode: ["fast", "slow"] }`
	const schema = `choices(p.via, ({ valueOf }) => (valueOf(p.link) === "tun" ? ["office", "vpn", "public"] : ["office", "public"]));
defaultValue(p.via, ({ valueOf }) => (valueOf(p.link) === "tun" ? "vpn" : "public"));
choices(p.mode, observed ? ["fast", "slow"] : ["slow"]);`
	cases := []struct {
		name string
		in   Input
		via  string
		from string
		mode string
	}{
		{"the default follows its source", Input{}, "vpn", "default", "slow"},
		{"and changes with it", Input{Session: map[string]string{"link": "none"}}, "public", "default", "slow"},
		{"an intent outside the current choices", Input{Session: map[string]string{"link": "none", "via": "vpn"}}, "public", "default", "slow"},
		{"comes back with them", Input{Session: map[string]string{"via": "vpn"}, Observed: &Observed{}}, "vpn", "session", "fast"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			form, _, err := resolved(t, model, schema, c.in)
			if err != nil {
				t.Fatal(err)
			}
			if via, mode := field(t, form, "via"), field(t, form, "mode"); via.Value != c.via || via.From != c.from || mode.Value != c.mode {
				t.Errorf("via = %q from %q, mode = %q; want %q from %q, %q", via.Value, via.From, mode.Value, c.via, c.from, c.mode)
			}
		})
	}
}

// A rule set on a group holds for everything it contains, and a hidden field keeps its value: the
// build receives it.
func TestARuleOnAGroupHoldsForAllItContains(t *testing.T) {
	t.Parallel()
	const model = `{ env: ["local", "staging"], api: { staging: ["office", "vpn"], mirror: false } }`
	const schema = `hidden(p.api, { when: ({ valueOf }) => valueOf(p.env) === "local" });
network(p.api);
disabled(p.api.mirror, { when: !observed && "nothing is observed" });
disabled(p.api.mirror, { when: true });`
	form, values, err := resolved(t, model, schema, Input{Session: map[string]string{"api.staging": "vpn"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api.staging", "api.mirror"} {
		if got := field(t, form, name); !got.Hidden || !got.Network {
			t.Errorf("%s = %+v, want hidden and following the network", name, got)
		}
	}
	if mirror := field(t, form, "api.mirror"); !mirror.Disabled || !reflect.DeepEqual(mirror.Reasons, []string{"nothing is observed"}) {
		t.Errorf("mirror = %+v, want disabled, with its only written reason", mirror)
	}
	if want := map[string]any{"env": "local", "api": map[string]any{"staging": "vpn", "mirror": false}}; !reflect.DeepEqual(values, want) {
		t.Errorf("the build received %v, want %v", values, want)
	}
	form, _, err = resolved(t, model, schema, Input{Session: map[string]string{"env": "staging"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := field(t, form, "api.staging"); got.Hidden {
		t.Errorf("api.staging = %+v, want visible outside local", got)
	}
}

// A schema applies to two groups and knows which one it is in; a block of rules holds only under its
// condition.
func TestSchemasApplyToPathsAndUnderConditions(t *testing.T) {
	t.Parallel()
	const model = `{ env: ["local", "staging"], front: { mirror: false }, back: { mirror: false }, delay: 0 }`
	const schema = `const backend = schema((group) => {
  disabled(group.mirror, { when: ({ pathKeys }) => pathKeys()[0] === "front" && "the front has no mirror" });
});
apply(p.front, backend);
apply(p.back, backend);
applyWhen(p, ({ valueOf }) => valueOf(p.env) === "staging", (p) => readonly(p.delay));
applyWhenValue(p.env, (env) => env === "local", () => keep(p.delay));`
	cases := []struct {
		env            string
		readonly, kept bool
	}{{"local", false, true}, {"staging", true, false}}
	for _, c := range cases {
		form, _, err := resolved(t, model, schema, Input{Session: map[string]string{"env": c.env}})
		if err != nil {
			t.Fatal(err)
		}
		if front, back := field(t, form, "front.mirror"), field(t, form, "back.mirror"); !front.Disabled || back.Disabled {
			t.Errorf("front = %+v, back = %+v: the schema does not know where it is", front, back)
		}
		if delay := field(t, form, "delay"); delay.Readonly != c.readonly || delay.Keep != c.kept {
			t.Errorf("env=%s: delay = %+v, want readonly=%v keep=%v", c.env, delay, c.readonly, c.kept)
		}
	}
}

// The built-in validators write their message, or the rule's; a field nobody can set is not validated,
// and a form with an error builds nothing.
func TestValidatorsSayWhatIsWrong(t *testing.T) {
	t.Parallel()
	const model = `{ delay: 0, branch: "", name: "ok", lock: false }`
	const schema = `min(p.delay, 0); max(p.delay, 10000, { message: "ten seconds at most" });
required(p.branch, { when: ({ valueOf }) => valueOf(p.delay) > 100 });
pattern(p.branch, /^[a-z-]*$/);
minLength(p.name, 2); maxLength(p.name, 4);
readonly(p.name, { when: ({ valueOf }) => valueOf(p.lock) });`
	cases := []struct {
		name   string
		asked  map[string]string
		field  string
		errors []string
	}{
		{"below", map[string]string{"delay": "-1"}, "delay", []string{"at least 0"}},
		{"above, by the rule", map[string]string{"delay": "20000"}, "delay", []string{"ten seconds at most"}},
		{"required under a condition", map[string]string{"delay": "200"}, "branch", []string{"required"}},
		{"pattern", map[string]string{"branch": "Main"}, "branch", []string{"does not match /^[a-z-]*$/"}},
		{"too short", map[string]string{"name": "a"}, "name", []string{"at least 2 characters"}},
		{"too long", map[string]string{"name": "abcde"}, "name", []string{"at most 4 characters"}},
		{"read-only, not validated", map[string]string{"name": "abcde", "lock": "true"}, "name", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			form, values, err := resolved(t, model, schema, Input{Asked: c.asked})
			if err != nil {
				t.Fatal(err)
			}
			if got := field(t, form, c.field); !slices.Equal(got.Errors, c.errors) {
				t.Errorf("%s: errors %q, want %q", c.field, got.Errors, c.errors)
			}
			if built := values != nil; built != (c.errors == nil) {
				t.Errorf("configuration built: %v, with the errors %q", built, form.Problems())
			}
		})
	}
	form, _, err := resolved(t, model, schema, Input{})
	if err != nil {
		t.Fatal(err)
	}
	if delay := field(t, form, "delay"); delay.Min == nil || *delay.Min != 0 || *delay.Max != 10000 {
		t.Errorf("delay = %+v, want its bounds for whoever types it", delay)
	}
	if name := field(t, form, "name"); *name.MinLength != 2 || *name.MaxLength != 4 || field(t, form, "branch").Pattern != "^[a-z-]*$" {
		t.Errorf("name = %+v, want its lengths, and the pattern of branch", name)
	}
}

// validate and validateTree may be async; a tree error attached to a hidden field is dropped, and without
// a field it belongs to the form.
func TestTreesAndAsyncRulesAreWaitedFor(t *testing.T) {
	t.Parallel()
	const model = `{ locale: ["en", "fr"], via: ["office", "public"], secret: "" }`
	const schema = `validate(p.via, async ({ value }) => { await new Promise((done) => setTimeout(done, 20)); return value() === "public" && "public is down"; });
hidden(p.secret, { when: ({ valueOf }) => valueOf(p.locale) === "en" });
validateTree(p, async ({ valueOf }) => [
  valueOf(p.locale) === "fr" && valueOf(p.via) === "office" && { field: p.locale, message: "office has no fr build" },
  { field: p.secret, message: "falls with its hidden field" },
]);
validateTree(p, ({ valueOf }) => valueOf(p.via) === "office" && valueOf(p.locale) === "fr" && "the form as a whole");`
	form, values, err := resolved(t, model, schema, Input{Asked: map[string]string{"locale": "fr"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"locale: office has no fr build", "secret: falls with its hidden field", "the form as a whole"}; !reflect.DeepEqual(form.Problems(), want) || values != nil {
		t.Errorf("problems %q, want %q and nothing built", form.Problems(), want)
	}
	form, values, err = resolved(t, model, schema, Input{Asked: map[string]string{"via": "public"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"via: public is down"}; !reflect.DeepEqual(form.Problems(), want) || values != nil {
		t.Errorf("problems %q, want %q: the error of secret is dropped with it", form.Problems(), want)
	}
}

// A retained value that the rules of its field no longer accept does not invalidate the form: it yields to
// the bound it crosses, to the default otherwise, and the field says so. An asked value, on the other
// hand, remains an error, and so does a default that does not hold.
func TestARetainedValueThatNoLongerHoldsYields(t *testing.T) {
	t.Parallel()
	const model = `{ api: ["local", "staging"], discount: 0, client: "" }`
	const schema = `keep(p.discount); min(p.discount, 0); max(p.discount, 15, { message: "the shop would sell at a loss" });
validate(p.client, ({ value }) => value() === "al" && "not an account"); required(p.client, { when: ({ valueOf }) => valueOf(p.api) === "staging" });
validateTree(p, ({ valueOf }) => valueOf(p.api) === "staging" && valueOf(p.discount) > 5 ? { field: p.discount, message: "no discount on staging" } : null);`
	cases := []struct {
		name          string
		in            Input
		field, value  string
		yielded, said string
		built         bool
	}{
		{"above the ceiling", Input{Kept: map[string]string{"discount": "20"}}, "discount", "15", "20",
			"discount=20 no longer holds (the shop would sell at a loss): discount=15 until it does", true},
		{"below the floor", Input{Session: map[string]string{"discount": "-3"}}, "discount", "0", "-3",
			"discount=-3 no longer holds (at least 0): discount=0 until it does", true},
		{"a tree rule, to the default, under a gesture", Input{Kept: map[string]string{"discount": "10"}, Asked: map[string]string{"api": "staging", "client": "alice"}},
			"discount", "0", "10", "discount=10 no longer holds (no discount on staging): discount=0 until it does", true},
		{"holds again", Input{Kept: map[string]string{"discount": "10"}}, "discount", "10", "", "", true},
		{"asked, it remains an error", Input{Asked: map[string]string{"discount": "20"}}, "discount", "20", "", "", false},
		{"the default does not hold either", Input{Session: map[string]string{"api": "staging", "client": "al"}}, "client", "", "al",
			"client=al no longer holds (not an account): client= until it does", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			form, values, err := resolved(t, model, schema, c.in)
			if err != nil {
				t.Fatal(err)
			}
			got := field(t, form, c.field)
			if got.Value != c.value || got.Yields() != c.said {
				t.Errorf("%s = %q, %q; want %q, %q", c.field, got.Value, got.Yields(), c.value, c.said)
			}
			if got.Yielded != nil && (got.Yielded.Value != c.yielded || got.From != "default") {
				t.Errorf("%s: yielded %+v from %q, want %q, and default", c.field, got.Yielded, got.From, c.yielded)
			}
			if built := values != nil; built != c.built {
				t.Errorf("configuration built: %v, want %v (%q)", built, c.built, form.Problems())
			}
		})
	}
}

// What a schema cannot mean is an evaluation error, which changes nothing: a cycle is named by the fields
// it goes through.
func TestAnImpossibleSchemaIsAnError(t *testing.T) {
	t.Parallel()
	const model = `{ a: ["x", "y"], b: ["x", "y"], group: { c: 0 } }`
	for schema, said := range map[string]string{
		`defaultValue(p.a, ({ valueOf }) => valueOf(p.b)); defaultValue(p.b, ({ valueOf }) => valueOf(p.a));`: "cycle: a → b → a",
		`hidden(p.a, { when: async () => true });`:                                                            "a: only validate and validateTree may be async",
		`hidden(p.deploymen);`: "hidden(): no field named deploymen",
		`min(p.group, 0);`:     "min(group): the rule is on one field, not on a group",
		`choices(p.a, []);`:    "a: its choices leave none",
	} {
		_, _, err := resolved(t, model, schema, Input{})
		if err == nil || !strings.Contains(err.Error(), said) {
			t.Errorf("%s: %v, want an error that says %q", schema, err, said)
		}
	}
}

// acceptance is the example of the design, which each refusal of set describes.
const acceptance = `{ env: ["local", "staging"], api: { deployment: ["alpha", "beta", "gamma"], mirror: false }, locale: ["en", "fr"], delay: 0 }`

const acceptanceRules = `keep(p.env); keep(p.locale);
hidden(p.api, { when: ({ valueOf }) => valueOf(p.env) === "local" });
network(p.api);
const office = observed?.exits.some((exit) => exit.via === "192.0.2.1") ?? false;
disabled(p.api.mirror, { when: () => !office && "the mirror is only reachable from the office network" });
choices(p.api.deployment, office ? ["alpha", "beta", "gamma"] : ["alpha", "beta"]);
readonly(p.delay, { when: ({ valueOf }) => valueOf(p.locale) === "fr" });
max(p.delay, 10000);
validateTree(p, ({ valueOf }) => valueOf(p.locale) === "fr" && valueOf(p.api.deployment) === "beta" ? { field: p.locale, message: "beta has no fr build" } : null);
validateTree(p, ({ valueOf }) => valueOf(p.env) === "staging" && valueOf(p.delay) > 5000 ? "staging answers within 5 s" : null);`

// Accept is the only guard of writes: each refusal is said as set writes it, and fields asked
// together are evaluated together.
func TestAcceptSaysWhyAValueIsNotTaken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      Input
		refused string
	}{
		{"unknown", Input{Asked: map[string]string{"x": "1"}}, `no parameter named "x" (declared: env, api.deployment, api.mirror, locale, delay)`},
		{"hidden", Input{Asked: map[string]string{"api.deployment": "beta"}}, "api.deployment is hidden here"},
		{"disabled", Input{Session: map[string]string{"env": "staging"}, Asked: map[string]string{"api.mirror": "true"}},
			"api.mirror is disabled: the mirror is only reachable from the office network"},
		{"read-only", Input{Session: map[string]string{"locale": "fr"}, Asked: map[string]string{"delay": "5"}}, "delay is read-only here"},
		{"outside the current choices", Input{Asked: map[string]string{"env": "prod"}}, `env: "prod" is not one of local, staging`},
		{"outside the choices of this network", Input{Session: map[string]string{"env": "staging"}, Asked: map[string]string{"api.deployment": "gamma"}},
			`api.deployment: "gamma" is not one of alpha, beta`},
		{"unreadable", Input{Asked: map[string]string{"delay": "abc"}}, `delay: "abc" is not a number`},
		{"invalid", Input{Asked: map[string]string{"delay": "20000"}}, "delay: at most 10000"},
		{"the form", Input{Session: map[string]string{"env": "staging"}, Asked: map[string]string{"delay": "6000"}},
			"the form refuses it: staging answers within 5 s"},
		// What the session had chosen yields to the gesture: locale falls back to en.
		{"a retained value that no longer holds", Input{Session: map[string]string{"env": "staging", "locale": "fr"}, Asked: map[string]string{"api.deployment": "beta"}}, ""},
		{"the asked field", Input{Session: map[string]string{"env": "staging", "api.deployment": "beta"}, Asked: map[string]string{"locale": "fr"}},
			"locale: beta has no fr build"},
		{"the pair together", Input{Asked: map[string]string{"env": "staging", "api.deployment": "beta"}}, ""},
		{"without a request, a valid form", Input{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			form, _, err := resolved(t, acceptance, acceptanceRules, c.in)
			if err != nil {
				t.Fatal(err)
			}
			switch err := form.Accept(c.in.Asked); {
			case c.refused == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.refused != "" && (err == nil || err.Error() != c.refused):
				t.Errorf("Accept = %v, want %q", err, c.refused)
			}
		})
	}
}

// What an accepted gesture keeps, and where: the two rules are independent, and read on the form the
// gesture produced.
func TestKeepingAndFollowingAreRulesOfTheForm(t *testing.T) {
	t.Parallel()
	asked := map[string]string{"env": "staging", "api.deployment": "beta", "locale": "en"}
	form, _, err := resolved(t, acceptance, acceptanceRules, Input{Asked: asked})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := form.Keeping(asked), map[string]string{"env": "staging", "locale": "en"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Keeping = %v, want %v", got, want)
	}
	if got, want := form.Following(asked), map[string]string{"api.deployment": "beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Following = %v, want %v", got, want)
	}
}
