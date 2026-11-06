package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Form is the resolved state of the parameters: what an interface shows, and what a gesture can
// write. It comes out of every evaluation, whole, and none of it is stored anywhere but in the session
// that asked for it.
type Form struct {
	Fields []Field `json:"fields"`
	// Errors are the form's errors that no field carries.
	Errors []string `json:"errors"`
	// Notices are what the form says without refusing anything, and that no shown field carries: a
	// build that depends on a hidden field.
	Notices []string `json:"notices"`
}

// MarshalJSON writes its fields and its errors, even when there are none: a tool has nothing to guess
// from a missing key.
func (f Form) MarshalJSON() ([]byte, error) {
	type plain Form
	f.Fields, f.Errors, f.Notices = append([]Field{}, f.Fields...), append([]string{}, f.Errors...), append([]string{}, f.Notices...)
	return json.Marshal(plain(f))
}

// Field is a field of the form, as the rules made it for this evaluation. A dotted name
// (api.deployment) gives the group it lives in; the order is the model's.
type Field struct {
	Name string `json:"name"`
	// Type is choice, flag, text or number.
	Type string `json:"type"`
	// Choices are the current ones; a flag always has its own, a text or a number has some only through
	// a rule.
	Choices []string `json:"choices,omitzero"`
	// DisabledChoices are the choices that cannot be taken here, each with its reason, empty when the
	// rule gives none: they stay in sight, and the value never takes them.
	DisabledChoices map[string]string `json:"disabledChoices,omitzero"`
	Value           string            `json:"value"`
	// From says where the value comes from: asked, network, session, kept or default.
	From      string   `json:"from"`
	Hidden    bool     `json:"hidden,omitzero"`
	Disabled  bool     `json:"disabled,omitzero"`
	Reasons   []string `json:"reasons,omitzero"`
	Readonly  bool     `json:"readonly,omitzero"`
	Required  bool     `json:"required,omitzero"`
	Min       *float64 `json:"min,omitzero"`
	Max       *float64 `json:"max,omitzero"`
	MinLength *int     `json:"minLength,omitzero"`
	MaxLength *int     `json:"maxLength,omitzero"`
	Pattern   string   `json:"pattern,omitzero"`
	Keep      bool     `json:"keep,omitzero"`
	Network   bool     `json:"network,omitzero"`
	Errors    []string `json:"errors"`
	// Yielded is the kept intention that the field's rules no longer take. Value replaces it as long as
	// it does not hold — the bound it exceeds, otherwise the default, and From says default — and it
	// comes back as soon as it holds: nothing erases it from memory.
	Yielded *Yield `json:"yielded,omitzero"`
}

// Yield is a kept intention that no longer holds: its value, the layer it comes from — network,
// session or kept — and what the field's rules said about it.
type Yield struct {
	Value  string   `json:"value"`
	From   string   `json:"from"`
	Errors []string `json:"errors"`
}

// Yields says in one line the kept intention the field no longer takes, and what replaces it: the
// sentence of the log, the state and the interfaces. Nothing when it holds.
func (f Field) Yields() string {
	if f.Yielded == nil {
		return ""
	}
	return fmt.Sprintf("%s=%s no longer holds (%s): %s=%s until it does", f.Name, f.Yielded.Value, strings.Join(f.Yielded.Errors, "; "), f.Name, f.Value)
}

// Next returns the choice after the value, wrapping around, skipping the disabled ones: the panel's
// and the TUI's gesture on a flag or a choice.
func (f Field) Next() string {
	at := slices.Index(f.Choices, f.Value)
	for step := 1; step < len(f.Choices); step++ {
		if next := f.Choices[(at+step)%len(f.Choices)]; f.Unavailable(next) == "" {
			return next
		}
	}
	return f.Value
}

// Unavailable says that one of the field's choices is disabled here, and why; nothing when it can be
// chosen.
func (f Field) Unavailable(choice string) string {
	why, off := f.DisabledChoices[choice]
	switch {
	case !off:
		return ""
	case why == "":
		return choice + " is disabled here"
	}
	return choice + " is disabled here: " + why
}

// Unavailables says, in the order of the choices, each of those disabled here.
func (f Field) Unavailables() []string {
	var said []string
	for _, choice := range f.Choices {
		if off := f.Unavailable(choice); off != "" {
			said = append(said, off)
		}
	}
	return said
}

// Group returns the group the field lives in, and its name within that group: api.deployment is
// deployment, under api. A field outside any group has only its name.
func (f Field) Group() (group, name string) {
	at := strings.LastIndexByte(f.Name, '.')
	return f.Name[:max(at, 0)], f.Name[at+1:]
}

// Bounds says what an input to the field must respect, as its rules declare it: what an interface
// announces before anything is typed.
func (f Field) Bounds() []string {
	var said []string
	for _, bound := range []struct {
		value *float64
		said  string
	}{{f.Min, "at least %v"}, {f.Max, "at most %v"}} {
		if bound.value != nil {
			said = append(said, fmt.Sprintf(bound.said, *bound.value))
		}
	}
	for _, bound := range []struct {
		value *int
		said  string
	}{{f.MinLength, "at least %d characters"}, {f.MaxLength, "at most %d characters"}} {
		if bound.value != nil {
			said = append(said, fmt.Sprintf(bound.said, *bound.value))
		}
	}
	return said
}

// Field returns the field of that name.
func (f Form) Field(name string) (Field, bool) {
	i := slices.IndexFunc(f.Fields, func(field Field) bool { return field.Name == name })
	if i < 0 {
		return Field{}, false
	}
	return f.Fields[i], true
}

// Accept says whether these asked values were taken by this form, the one they produced: a declared
// and interactive field, a value readable in its type, among its choices, taken, and a form without
// error. It is the only guard of writes, and an invalid form builds nothing: with no asked value,
// Accept therefore says whether the configuration was built.
func (f Form) Accept(asked map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(asked)) {
		field, declared := f.Field(name)
		value := asked[name]
		switch {
		case !declared:
			return fmt.Errorf("no parameter named %q (declared: %s)", name, f.names())
		case field.Hidden:
			return fmt.Errorf("%s is hidden here", name)
		case field.Disabled && len(field.Reasons) == 0:
			return fmt.Errorf("%s is disabled here", name)
		case field.Disabled:
			return fmt.Errorf("%s is disabled: %s", name, strings.Join(field.Reasons, "; "))
		case field.Readonly:
			return fmt.Errorf("%s is read-only here", name)
		case field.Choices != nil && !slices.Contains(field.Choices, value):
			return fmt.Errorf("%s: %q is not one of %s", name, value, strings.Join(field.Choices, ", "))
		case field.Unavailable(value) != "":
			return fmt.Errorf("%s: %s", name, field.Unavailable(value))
		case field.From != "asked" && field.Type == "number":
			return fmt.Errorf("%s: %q is not a number", name, value)
		case field.From != "asked" || field.Value != value:
			return fmt.Errorf("%s: the form took %q, not %q", name, field.Value, value)
		case len(field.Errors) > 0:
			return fmt.Errorf("%s: %s", name, strings.Join(field.Errors, "; "))
		}
	}
	if problems := f.Problems(); len(problems) > 0 {
		return fmt.Errorf("the form refuses it: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Problems returns what makes the form invalid, each field error preceded by its name: what the log,
// the state and a refusal write.
func (f Form) Problems() []string {
	var out []string
	for _, field := range f.Fields {
		for _, err := range field.Errors {
			out = append(out, field.Name+": "+err)
		}
	}
	return append(out, f.Errors...)
}

// Said returns everything the form says without refusing: each value that yields, then its notices.
// It is what the log tells when things start.
func (f Form) Said() []string {
	return append(f.Yields(), f.Notices...)
}

// Yields returns the sentence of each field whose kept intention no longer holds, in the model's
// order.
func (f Form) Yields() []string {
	var said []string
	for _, field := range f.Fields {
		if yields := field.Yields(); yields != "" {
			said = append(said, yields)
		}
	}
	return said
}

// Keeping and Following return those of these values that the form keeps for the project, or for the
// network: what an accepted gesture writes, and nowhere else.
func (f Form) Keeping(values map[string]string) map[string]string {
	return f.carried(values, func(field Field) bool { return field.Keep })
}

func (f Form) Following(values map[string]string) map[string]string {
	return f.carried(values, func(field Field) bool { return field.Network })
}

// Shown returns the value of each field an interface shows: a hidden field exists for none of them.
func (f Form) Shown() map[string]string {
	values := map[string]string{}
	for _, field := range f.Fields {
		if !field.Hidden {
			values[field.Name] = field.Value
		}
	}
	return values
}

func (f Form) carried(values map[string]string, rule func(Field) bool) map[string]string {
	carried := map[string]string{}
	for name, value := range values {
		if field, declared := f.Field(name); declared && rule(field) {
			carried[name] = value
		}
	}
	return carried
}

func (f Form) names() string {
	names := make([]string, len(f.Fields))
	for i, field := range f.Fields {
		names[i] = field.Name
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

var (
	fieldName = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)
	types     = []string{"choice", "flag", "text", "number"}
	origins   = []string{"asked", "network", "session", "kept", "default"}
	retained  = []string{"network", "session", "kept"}
)

// validate guards the invariant of what a program writes: well-formed and unique names, a known type,
// choices that are words, a value among them, a known origin, and single-line texts.
func (f Form) validate() []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	declared := map[string]bool{}
	for _, field := range f.Fields {
		switch {
		case !fieldName.MatchString(field.Name):
			fail("parameter %q: a name is made of letters, digits, _ and -, and a group joins them with dots", field.Name)
		case declared[field.Name]:
			fail("two parameters are named %q", field.Name)
		}
		declared[field.Name] = true
		switch {
		case !slices.Contains(types, field.Type):
			fail("%s: unknown type %q (%s)", field.Name, field.Type, strings.Join(types, ", "))
		case field.Type == "flag" && (len(field.Choices) != 2 || !slices.Contains(field.Choices, "false") || !slices.Contains(field.Choices, "true")):
			fail("%s: a flag chooses between false and true", field.Name)
		case field.Type == "choice" && len(field.Choices) == 0:
			fail("%s: no choice declared", field.Name)
		}
		for i, choice := range field.Choices {
			switch {
			case choice == "" || strings.ContainsFunc(choice, unicode.IsSpace):
				fail("%s: choice %q must be a word, neither empty nor holding a space", field.Name, choice)
			case slices.Contains(field.Choices[:i], choice):
				fail("%s: choice %q is declared twice", field.Name, choice)
			}
		}
		if field.Choices != nil && !slices.Contains(field.Choices, field.Value) {
			fail("%s: value %q is not one of %s", field.Name, field.Value, strings.Join(field.Choices, ", "))
		}
		for choice, why := range field.DisabledChoices {
			switch {
			case !slices.Contains(field.Choices, choice):
				fail("%s: disabled choice %q is not one of %s", field.Name, choice, strings.Join(field.Choices, ", "))
			case choice == field.Value:
				fail("%s: value %q is disabled", field.Name, choice)
			case strings.ContainsAny(why, "\r\n"):
				fail("%s: %q holds a line break", field.Name, why)
			}
		}
		if _, err := strconv.ParseFloat(field.Value, 64); field.Type == "number" && err != nil {
			fail("%s: value %q is not a number", field.Name, field.Value)
		}
		if !slices.Contains(origins, field.From) {
			fail("%s: unknown origin %q (%s)", field.Name, field.From, strings.Join(origins, ", "))
		}
		texts := append(append([]string{field.Value}, field.Reasons...), field.Errors...)
		if field.Yielded != nil {
			if !slices.Contains(retained, field.Yielded.From) {
				fail("%s: a value that yields comes from %s, not %q", field.Name, strings.Join(retained, ", "), field.Yielded.From)
			}
			texts = append(append(texts, field.Yielded.Value), field.Yielded.Errors...)
		}
		for _, text := range texts {
			if strings.ContainsAny(text, "\r\n") {
				fail("%s: %q holds a line break", field.Name, text)
			}
		}
	}
	for _, text := range append(slices.Clone(f.Errors), f.Notices...) {
		if strings.ContainsAny(text, "\r\n") {
			fail("form message %q holds a line break", text)
		}
	}
	return problems
}

// engine is the form engine, which the shim serves under the name "dsvs".
//
//go:embed form.js
var engine string

// quoted writes a text as a JavaScript literal: that is how the engine travels inside the shim.
func quoted(text string) string {
	out, _ := json.Marshal(text)
	return string(out)
}
