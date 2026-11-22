package project

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/spec"
)

// ConfigFile is tasks.toml's path relative to the repository root.
const ConfigFile = Dir + "/tasks.toml"

// Config is tasks.toml.
type Config struct {
	Format     int                  `toml:"format"`
	Project    ProjectConfig        `toml:"project"`
	Components map[string]Component `toml:"components"`
	Routing    map[string]string    `toml:"routing"`
}

// ProjectConfig is tasks.toml's [project].
type ProjectConfig struct {
	IDPrefix   string `toml:"id_prefix"`
	MainBranch string `toml:"main_branch"`
}

// Component is one [components.<name>].
type Component struct {
	Paths   []string `toml:"paths"`
	Release Release  `toml:"release"`
}

// Release is a component's release rule.
type Release struct {
	Rule    string  `toml:"rule"`
	Pattern *string `toml:"pattern"`
}

// Rule values.
const (
	RuleTag   = "tag"
	RuleMerge = "merge"
)

var (
	idPrefixRe    = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)
	componentRe   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	profileNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// Kinds are the task kinds of the format, also the keys [routing] may use
// besides "default".
var Kinds = []string{"feature", "fix", "chore", "research", "design"}

// ParseConfig decodes tasks.toml strictly and checks what can be checked
// without git. The config is nil only when the file cannot be decoded at all.
func ParseConfig(data []byte) (*Config, []finding.Finding) {
	var cfg Config
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&cfg)
	if err != nil {
		line := 0
		var perr toml.ParseError
		if errors.As(err, &perr) {
			line = perr.Position.Line
			err = errors.New(perr.Message)
		}
		return nil, []finding.Finding{finding.Errorf(ConfigFile, line, "cannot parse: %v", err)}
	}
	var fs []finding.Finding
	// Unknown keys: the decoder matches field names case-insensitively, so
	// every key's exact spelling is checked too (Main_Branch is unknown).
	reported := map[string]bool{}
	unknown := func(k toml.Key) {
		if !reported[k.String()] {
			reported[k.String()] = true
			fs = append(fs, finding.Errorf(ConfigFile, keyLine(data, k), "unknown key %q", k.String()))
		}
	}
	for _, k := range md.Keys() {
		if !knownKey(k) {
			unknown(k)
		}
	}
	for _, k := range md.Undecoded() {
		unknown(k)
	}
	if !md.IsDefined("format") {
		fs = append(fs, finding.Errorf(ConfigFile, 0, "format is missing; this reader reads format = %d", spec.Format))
	} else if cfg.Format != spec.Format {
		fs = append(fs, finding.Errorf(ConfigFile, keyLine(data, toml.Key{"format"}), "format = %d; this reader reads format = %d", cfg.Format, spec.Format))
	}
	if !idPrefixRe.MatchString(cfg.Project.IDPrefix) {
		fs = append(fs, finding.Errorf(ConfigFile, keyLine(data, toml.Key{"project", "id_prefix"}), "project.id_prefix %q does not match %s", cfg.Project.IDPrefix, idPrefixRe))
	}
	if cfg.Project.MainBranch == "" {
		fs = append(fs, finding.Errorf(ConfigFile, keyLine(data, toml.Key{"project"}), "project.main_branch is not set"))
	}
	names := make([]string, 0, len(cfg.Components))
	for name := range cfg.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := cfg.Components[name]
		line := keyLine(data, toml.Key{"components", name})
		if !componentRe.MatchString(name) {
			fs = append(fs, finding.Errorf(ConfigFile, line, "component name %q does not match %s", name, componentRe))
		}
		if len(c.Paths) == 0 {
			fs = append(fs, finding.Errorf(ConfigFile, line, "component %s: paths is empty", name))
		}
		switch c.Release.Rule {
		case RuleTag:
			if c.Release.Pattern == nil || *c.Release.Pattern == "" {
				fs = append(fs, finding.Errorf(ConfigFile, line, "component %s: rule \"tag\" needs a pattern", name))
			}
		case RuleMerge:
			if c.Release.Pattern != nil {
				fs = append(fs, finding.Errorf(ConfigFile, line, "component %s: rule \"merge\" takes no pattern", name))
			}
		case "":
			fs = append(fs, finding.Errorf(ConfigFile, line, "component %s: release.rule is not set", name))
		default:
			fs = append(fs, finding.Errorf(ConfigFile, line, "component %s: release.rule %q is not \"tag\" or \"merge\"", name, c.Release.Rule))
		}
	}
	keys := make([]string, 0, len(cfg.Routing))
	for k := range cfg.Routing {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		line := keyLine(data, toml.Key{"routing", k})
		if k != "default" && !contains(Kinds, k) {
			fs = append(fs, finding.Errorf(ConfigFile, line, "routing.%s: not \"default\" or a task kind (%s)", k, strings.Join(Kinds, ", ")))
		}
		if v := cfg.Routing[k]; !profileNameRe.MatchString(v) {
			fs = append(fs, finding.Errorf(ConfigFile, line, "routing.%s: profile name %q does not match %s", k, v, profileNameRe))
		}
	}
	return &cfg, fs
}

// Pattern returns the component's tag pattern, "" for none.
func (r Release) PatternString() string {
	if r.Pattern == nil {
		return ""
	}
	return *r.Pattern
}

// knownKey reports whether a key, spelt exactly, belongs to tasks.toml.
func knownKey(k toml.Key) bool {
	switch len(k) {
	case 1:
		return k[0] == "format" || k[0] == "project" || k[0] == "components" || k[0] == "routing"
	case 2:
		switch k[0] {
		case "project":
			return k[1] == "id_prefix" || k[1] == "main_branch"
		case "components", "routing":
			return true // names; checked separately
		}
	case 3:
		return k[0] == "components" && (k[2] == "paths" || k[2] == "release")
	case 4:
		return k[0] == "components" && k[2] == "release" && (k[3] == "rule" || k[3] == "pattern")
	}
	return false
}

// keyLine finds, best effort, the line that defines key: a table header
// naming it or an assignment of its last part. 0 when not found.
func keyLine(data []byte, key toml.Key) int {
	if len(key) == 0 {
		return 0
	}
	last := key[len(key)-1]
	header := "[" + strings.Join(key, ".") + "]"
	lines := strings.Split(string(data), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == header {
			return i + 1
		}
	}
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, last) {
			rest := strings.TrimSpace(strings.TrimPrefix(t, last))
			if strings.HasPrefix(rest, "=") {
				return i + 1
			}
		}
		if strings.Contains(t, last+" =") || strings.Contains(t, last+"=") {
			if strings.Contains(t, "{") {
				return i + 1
			}
		}
	}
	return 0
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// String describes the release rule for messages.
func (r Release) String() string {
	if r.Rule == RuleTag {
		return fmt.Sprintf("a %s tag", r.PatternString())
	}
	return "the main branch"
}
