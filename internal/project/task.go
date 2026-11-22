package project

import (
	"encoding/json"
	"fmt"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/schema"
)

// MaxTouches bounds a task's touches list, so the pairwise overlap check
// (run under the project lock by writers) stays small.
const MaxTouches = 1000

// Dir is the task directory, relative to the repository root (fixed in
// format 1).
const Dir = "tasks"

// Task is a parsed task file.
type Task struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Kind         string   `json:"kind"`
	Status       string   `json:"status"`
	Priority     int      `json:"priority"`
	Component    string   `json:"component"`
	DependsOn    []string `json:"depends_on"`
	Touches      []string `json:"touches"`
	Harness      string   `json:"harness"`
	Reviews      Reviews  `json:"reviews"`
	Created      string   `json:"created"`
	NeedsPerson  bool     `json:"needs_person,omitempty"`
	AfterRelease string   `json:"after_release,omitempty"`
	Profile      string   `json:"profile,omitempty"`
	Model        string   `json:"model,omitempty"`

	// File is the path relative to the repository root.
	File string `json:"-"`
	// Body is everything after the front matter.
	Body string `json:"-"`
	// lines maps a JSON pointer of the front matter to its line in the file.
	lines map[string]int
}

// Reviews is the front matter's review budget.
type Reviews struct {
	Spec int `json:"spec"`
	Code int `json:"code"`
}

// Line returns the file line of the value at a JSON pointer, or of its
// nearest ancestor; 1 (the front matter's opening line) when unknown.
func (t *Task) Line(pointer string) int {
	for {
		if l, ok := t.lines[pointer]; ok {
			return l
		}
		i := strings.LastIndexByte(pointer, '/')
		if i < 0 {
			return 1
		}
		pointer = pointer[:i]
	}
}

var (
	fileNameRe = regexp.MustCompile(`^([A-Z][A-Z0-9]*-[0-9]{3,})-([a-z0-9]+(?:-[a-z0-9]+)*)\.md$`)
	// TaskIDRe is the shape of a task id.
	TaskIDRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]{3,}$`)
)

// IsTaskFile reports whether a file directly in tasks/ is meant as a task
// file: every *.md except README.md.
func IsTaskFile(name string) bool {
	return strings.HasSuffix(name, ".md") && name != "README.md"
}

// FileID returns the id a task file name carries, ok=false when the name
// does not have the `<ID>-<slug>.md` shape.
func FileID(name string) (string, bool) {
	m := fileNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ParseTask reads a task file. file is the path relative to the repository
// root. A nil task means the front matter could not be read or failed the
// schema; the findings say why.
func ParseTask(file string, data []byte, sch *schema.Schemas) (*Task, []finding.Finding) {
	var fs []finding.Finding
	name := path.Base(file)
	fileID, okName := FileID(name)
	if !okName {
		fs = append(fs, finding.Errorf(file, 0, "file name is not <ID>-<slug>.md (slug ^[a-z0-9]+(-[a-z0-9]+)*$)"))
	}
	front, frontLine, body, ok := splitFrontMatter(string(data))
	if !ok {
		return nil, append(fs, finding.Errorf(file, 1, "no front matter: the file must start with a line --- and close it with another line ---"))
	}
	lines := map[string]int{}
	value, err := yamlToJSON(front, frontLine, lines)
	if err != nil {
		return nil, append(fs, finding.Errorf(file, err.line, "front matter: %s", err.msg))
	}
	t := &Task{File: file, Body: body, lines: lines}
	if vs := schema.Validate(sch.Task, value); len(vs) > 0 {
		for _, v := range vs {
			fs = append(fs, finding.Errorf(file, t.Line(v.Pointer), "front matter: %s", v))
		}
		return nil, fs
	}
	if obj, ok := value.(map[string]any); ok {
		if ts, ok := obj["touches"].([]any); ok && len(ts) > MaxTouches {
			return nil, append(fs, finding.Errorf(file, t.Line("/touches"), "touches has %d entries; a reader takes at most %d", len(ts), MaxTouches))
		}
	}
	raw, _ := json.Marshal(value)
	if err := json.Unmarshal(raw, t); err != nil {
		return nil, append(fs, finding.Errorf(file, 1, "front matter: %v", err))
	}
	if t.DependsOn == nil {
		t.DependsOn = []string{}
	}
	if okName && t.ID != fileID {
		fs = append(fs, finding.Errorf(file, t.Line("/id"), "id %s does not equal the file name's prefix %s", t.ID, fileID))
	}
	return t, fs
}

// splitFrontMatter returns the text between a first line `---` and the next
// line `---`, the 1-based line of the front matter's first line, and the
// rest.
func splitFrontMatter(s string) (front string, firstLine int, body string, ok bool) {
	s = strings.TrimPrefix(s, "\uFEFF")
	lines := strings.SplitAfter(s, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r\n") != "---" {
		return "", 0, "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r\n") == "---" {
			return strings.Join(lines[1:i], ""), 2, strings.Join(lines[i+1:], ""), true
		}
	}
	return "", 0, "", false
}

type yamlError struct {
	line int
	msg  string
}

var (
	yamlLineRe = regexp.MustCompile(`^yaml: line (\d+): (.*)$`)
	digitsRe   = regexp.MustCompile(`^[-+]?[0-9]+$`)
)

// yamlToJSON decodes YAML to a JSON value by hand from the node tree, so
// only JSON-compatible YAML is accepted: an unquoted date, anchors, aliases,
// merge keys and duplicate keys are errors. Lines are absolute file lines
// (offset is the file line of the YAML's first line).
func yamlToJSON(src string, offset int, lines map[string]int) (any, *yamlError) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		msg := err.Error()
		line := offset
		if m := yamlLineRe.FindStringSubmatch(msg); m != nil {
			n, _ := strconv.Atoi(m[1])
			line = offset + n - 1
			msg = m[2]
		} else {
			msg = strings.TrimPrefix(msg, "yaml: ")
		}
		return nil, &yamlError{line: line, msg: msg}
	}
	if doc.Kind == 0 { // empty document
		return map[string]any{}, nil
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, &yamlError{line: offset, msg: "not a single YAML document"}
	}
	c := converter{offset: offset - 1, lines: lines}
	return c.value(doc.Content[0], "")
}

type converter struct {
	offset int
	lines  map[string]int
}

func (c converter) at(n *yaml.Node) int { return n.Line + c.offset }

func (c converter) value(n *yaml.Node, ptr string) (any, *yamlError) {
	c.lines[ptr] = c.at(n)
	if n.Anchor != "" {
		return nil, &yamlError{c.at(n), fmt.Sprintf("anchor &%s: anchors and aliases are not allowed", n.Anchor)}
	}
	switch n.Kind {
	case yaml.AliasNode:
		return nil, &yamlError{c.at(n), "alias: anchors and aliases are not allowed"}
	case yaml.MappingNode:
		obj := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Anchor != "" {
				return nil, &yamlError{c.at(k), fmt.Sprintf("anchor &%s: anchors and aliases are not allowed", k.Anchor)}
			}
			if k.Kind == yaml.AliasNode || k.Tag == "!!merge" {
				return nil, &yamlError{c.at(k), "merge keys (<<) are not allowed"}
			}
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
				return nil, &yamlError{c.at(k), fmt.Sprintf("key %q is not a string", k.Value)}
			}
			if _, dup := obj[k.Value]; dup {
				return nil, &yamlError{c.at(k), fmt.Sprintf("duplicate key %q", k.Value)}
			}
			child := ptr + "/" + escapePointer(k.Value)
			val, err := c.value(v, child)
			if err != nil {
				return nil, err
			}
			obj[k.Value] = val
			c.lines[child] = c.at(k)
		}
		return obj, nil
	case yaml.SequenceNode:
		arr := make([]any, 0, len(n.Content))
		for i, item := range n.Content {
			val, err := c.value(item, ptr+"/"+strconv.Itoa(i))
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		return arr, nil
	case yaml.ScalarNode:
		return c.scalar(n)
	}
	return nil, &yamlError{c.at(n), "unsupported YAML node"}
}

func (c converter) scalar(n *yaml.Node) (any, *yamlError) {
	switch n.Tag {
	case "!!str":
		return n.Value, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, &yamlError{c.at(n), err.Error()}
		}
		return b, nil
	case "!!int":
		var i int64
		if err := n.Decode(&i); err != nil {
			if digitsRe.MatchString(n.Value) { // too big for int64: still a number
				return json.Number(strings.TrimLeft(n.Value, "+")), nil
			}
			return nil, &yamlError{c.at(n), fmt.Sprintf("integer %s: %v", n.Value, err)}
		}
		return json.Number(strconv.FormatInt(i, 10)), nil
	case "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, &yamlError{c.at(n), err.Error()}
		}
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, &yamlError{c.at(n), fmt.Sprintf("%s is not a JSON number", n.Value)}
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), nil
	case "!!timestamp":
		return nil, &yamlError{c.at(n), fmt.Sprintf("%s is an unquoted date; quote it: \"%s\"", n.Value, n.Value)}
	}
	return nil, &yamlError{c.at(n), fmt.Sprintf("value %q has YAML type %s, which is not JSON-compatible", n.Value, n.Tag)}
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
