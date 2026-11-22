// Package schema compiles the embedded task-format schemas and validates
// JSON values against them.
//
// The schemas use ECMA-262 regular expressions (the `touches` pattern has
// lookahead, which Go's RE2 `regexp` rejects), so patterns are compiled with
// github.com/dlclark/regexp2 in ECMAScript mode.
package schema

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/proshik/taskmoor/spec"
)

// Violation is one way a value fails a schema.
type Violation struct {
	// Pointer is the JSON pointer of the failing value ("" for the root).
	Pointer string
	Message string
}

func (v Violation) String() string {
	if v.Pointer == "" {
		return v.Message
	}
	return v.Pointer + ": " + v.Message
}

type ecmaRegexp regexp2.Regexp

func (re *ecmaRegexp) MatchString(s string) bool {
	ok, err := (*regexp2.Regexp)(re).MatchString(s)
	return err == nil && ok
}

func (re *ecmaRegexp) String() string { return (*regexp2.Regexp)(re).String() }

func compileECMA(s string) (jsonschema.Regexp, error) {
	re, err := regexp2.Compile(s, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	return (*ecmaRegexp)(re), nil
}

// Schemas holds the compiled task and state schemas.
type Schemas struct {
	Task  *jsonschema.Schema
	State *jsonschema.Schema
}

var (
	once     sync.Once
	compiled *Schemas
	errCompl error
)

// Load compiles the embedded schemas once.
func Load() (*Schemas, error) {
	once.Do(func() {
		compiled, errCompl = compile()
	})
	return compiled, errCompl
}

func compile() (*Schemas, error) {
	c := jsonschema.NewCompiler()
	c.UseRegexpEngine(compileECMA)
	for name, data := range map[string][]byte{
		"task.schema.json":  spec.TaskSchema,
		"state.schema.json": spec.StateSchema,
	} {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("embedded %s: %w", name, err)
		}
		if err := c.AddResource(name, doc); err != nil {
			return nil, fmt.Errorf("embedded %s: %w", name, err)
		}
	}
	task, err := c.Compile("task.schema.json")
	if err != nil {
		return nil, fmt.Errorf("embedded task.schema.json: %w", err)
	}
	state, err := c.Compile("state.schema.json")
	if err != nil {
		return nil, fmt.Errorf("embedded state.schema.json: %w", err)
	}
	return &Schemas{Task: task, State: state}, nil
}

var printer = message.NewPrinter(language.English)

// Validate checks value (a JSON value as produced by encoding/json or
// jsonschema.UnmarshalJSON) against sch and returns the leaf violations,
// sorted by pointer. nil means valid.
func Validate(sch *jsonschema.Schema, value any) []Violation {
	err := sch.Validate(value)
	if err == nil {
		return nil
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return []Violation{{Message: err.Error()}}
	}
	var out []Violation
	seen := map[string]bool{}
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			v := Violation{Pointer: pointer(e.InstanceLocation), Message: e.ErrorKind.LocalizedString(printer)}
			if key := v.String(); !seen[key] {
				seen[key] = true
				out = append(out, v)
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(verr)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Pointer < out[j].Pointer })
	return out
}

func pointer(loc []string) string {
	if len(loc) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range loc {
		b.WriteByte('/')
		p = strings.ReplaceAll(p, "~", "~0")
		b.WriteString(strings.ReplaceAll(p, "/", "~1"))
	}
	return b.String()
}
