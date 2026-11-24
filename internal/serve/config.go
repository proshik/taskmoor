// Package serve is `taskmoor serve`: the local service that launches agent
// sessions on ready tasks, keeps their journals, routes their questions to
// the person and serves the API its clients use.
package serve

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
)

// DefaultPort is where the service listens on 127.0.0.1 unless configured.
const DefaultPort = 7733

// Paths are the service's configuration file and store directory.
type Paths struct {
	Config string // config.toml
	Store  string // journals, asks, token, the serve lock
}

// DefaultPaths follows XDG: $XDG_CONFIG_HOME/taskmoor/config.toml and
// $XDG_STATE_HOME/taskmoor (defaults ~/.config and ~/.local/state).
// TASKMOOR_HOME, when set, holds both (config.toml and store/) — for tests
// and for running a second, separate service.
func DefaultPaths(getenv func(string) string) (Paths, error) {
	if h := getenv("TASKMOOR_HOME"); h != "" {
		return Paths{Config: filepath.Join(h, "config.toml"), Store: filepath.Join(h, "store")}, nil
	}
	home := getenv("HOME")
	if home == "" {
		return Paths{}, errors.New("HOME is not set")
	}
	cfg := getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	st := getenv("XDG_STATE_HOME")
	if st == "" {
		st = filepath.Join(home, ".local", "state")
	}
	return Paths{Config: filepath.Join(cfg, "taskmoor", "config.toml"), Store: filepath.Join(st, "taskmoor")}, nil
}

// Config is config.toml.
type Config struct {
	Port           int       `toml:"port"`
	DefaultProfile string    `toml:"default_profile"`
	Projects       []Project `toml:"projects"`
	Profiles       []Profile `toml:"profiles"`
	// Editor opens a worktree from the board: the command and its first
	// arguments, the worktree is added last (e.g. ["code", "-n"]).
	Editor []string `toml:"editor,omitempty"`
	// DispatcherProfile is the claude profile of the dispatcher's short
	// call (A13); empty: the dispatcher is off, the person answers all.
	DispatcherProfile string `toml:"dispatcher_profile,omitempty"`
	// ReviewCommand and ReviewProfile run the code review (A5): revust
	// and its codex-only profile unless set.
	ReviewCommand string `toml:"review_command,omitempty"`
	ReviewProfile string `toml:"review_profile,omitempty"`
	// UsageThreshold is the plan's share left (percent) below which new
	// automatic starts wait and manual ones need --anyway (default 10;
	// 0 turns the guard off).
	UsageThreshold *int `toml:"usage_threshold,omitempty"`
	// RemoteHost and RemotePort are the web from outside (stage 9): the
	// VPS's domain, and the port on 127.0.0.1 its reverse tunnel reaches.
	RemoteHost string `toml:"remote_host,omitempty"`
	RemotePort int    `toml:"remote_port,omitempty"`
}

// Project is a registered repository.
type Project struct {
	Name string `toml:"name"`
	Path string `toml:"path"`
	// KeepBusy is "Keep N busy" (A4): the sessions the service keeps
	// under way for the project; 0 is off.
	KeepBusy int `toml:"keep_busy,omitempty"`
}

// Profile is an agent profile.
type Profile struct {
	Name    string `toml:"name"`
	Harness string `toml:"harness"`
	Model   string `toml:"model,omitempty"`
	// Description is what the profile is good for, which the dispatcher
	// reads when it suggests one (stage 13).
	Description string `toml:"description,omitempty"`
	// APIBilling allows a session of this profile to bill an API key.
	APIBilling bool `toml:"api_billing,omitempty"`
	// Command overrides the harness program (default: claude; codex:
	// `npx -y` CodexACP; opencode: `opencode acp`), e.g. a wrapper, a
	// globally installed codex-acp or a test double.
	Command string `toml:"command,omitempty"`
	// Args are the command's arguments (an ACP agent's; opencode's default
	// is "acp").
	Args []string `toml:"args,omitempty"`
	// Sandbox confines the agent's commands (C13); on unless set to false.
	Sandbox *bool `toml:"sandbox,omitempty"`
	// AllowedDomains are the hosts the agent's commands may reach (default
	// DefaultAllowedDomains; an empty list allows none).
	// No omitempty: an explicit empty list must survive a save.
	AllowedDomains []string `toml:"allowed_domains"`
}

// Confined reports whether the profile's sessions run in the sandbox.
func (p Profile) Confined() bool {
	return p.Harness == "claude" && (p.Sandbox == nil || *p.Sandbox)
}

// Isolation names what confines the agent's commands: "sandbox" (Claude's,
// C13), "codex" (Codex's workspace-write sandbox: writes in the worktree,
// no network, reads not confined) or "" (unconfined).
func (p Profile) Isolation() string {
	switch {
	case p.Confined():
		return "sandbox"
	case p.Harness == "codex":
		return "codex"
	}
	return ""
}

// acpArgv is an ACP profile's command line.
func (p Profile) acpArgv() []string {
	switch {
	case p.Command != "":
		args := p.Args
		if args == nil && p.Harness == "opencode" {
			args = []string{"acp"}
		}
		return append([]string{p.Command}, args...)
	case p.Harness == "codex":
		return []string{"npx", "-y", CodexACP}
	case p.Harness == "opencode":
		return []string{"opencode", "acp"}
	}
	return nil
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Harnesses the service can run in this stage.
var runnable = map[string]bool{"claude": true, "codex": true, "opencode": true}

// LoadConfig reads config.toml; a missing file is an empty configuration
// with one default profile, `claude`.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultConfig(), nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%s: unknown key %q", path, und[0].String())
	}
	if len(c.Profiles) == 0 {
		c.Profiles = defaultConfig().Profiles
	}
	if c.DefaultProfile == "" {
		c.DefaultProfile = c.Profiles[0].Name
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	return &c, c.Check(path)
}

func defaultConfig() *Config {
	return &Config{Port: DefaultPort, DefaultProfile: "claude", Profiles: []Profile{{Name: "claude", Harness: "claude"}}}
}

// Check validates the configuration; path names it in messages.
func (c *Config) Check(path string) error {
	if (c.RemoteHost == "") != (c.RemotePort == 0) {
		return fmt.Errorf("%s: remote_host and remote_port go together", path)
	}
	if c.RemoteHost != "" {
		if strings.ContainsAny(c.RemoteHost, ":/ @") || strings.TrimSpace(c.RemoteHost) != c.RemoteHost {
			return fmt.Errorf("%s: remote_host is a domain name (no scheme, port or path), not %q", path, c.RemoteHost)
		}
		if c.RemotePort < 1 || c.RemotePort > 65535 || c.RemotePort == c.Port {
			return fmt.Errorf("%s: remote_port must be a free port other than port", path)
		}
	}
	seen := map[string]bool{}
	for _, p := range c.Profiles {
		if !nameRe.MatchString(p.Name) {
			return fmt.Errorf("%s: profile name %q does not match %s", path, p.Name, nameRe)
		}
		if seen[p.Name] {
			return fmt.Errorf("%s: profile %s is defined twice", path, p.Name)
		}
		seen[p.Name] = true
		switch p.Harness {
		case "claude", "codex", "opencode":
		default:
			return fmt.Errorf("%s: profile %s: harness %q is not claude, codex or opencode", path, p.Name, p.Harness)
		}
	}
	if c.Profile(c.DefaultProfile) == nil {
		return fmt.Errorf("%s: default_profile %q is not a profile", path, c.DefaultProfile)
	}
	if n := c.DispatcherProfile; n != "" {
		if p := c.Profile(n); p == nil || p.Harness != "claude" {
			return fmt.Errorf("%s: dispatcher_profile %q is not a claude profile", path, n)
		}
	}
	names := map[string]bool{}
	for _, p := range c.Projects {
		if !nameRe.MatchString(p.Name) {
			return fmt.Errorf("%s: project name %q does not match %s", path, p.Name, nameRe)
		}
		if names[p.Name] {
			return fmt.Errorf("%s: project %s is registered twice", path, p.Name)
		}
		names[p.Name] = true
		if !filepath.IsAbs(p.Path) {
			return fmt.Errorf("%s: project %s: path %q is not absolute", path, p.Name, p.Path)
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("%s: port %d is out of range", path, c.Port)
	}
	return nil
}

// Profile returns the profile with name, nil when there is none.
func (c *Config) Profile(name string) *Profile {
	for i := range c.Profiles {
		if c.Profiles[i].Name == name {
			return &c.Profiles[i]
		}
	}
	return nil
}

// ProjectByName returns the registered project, nil when there is none.
func (c *Config) ProjectByName(name string) *Project {
	for i := range c.Projects {
		if c.Projects[i].Name == name {
			return &c.Projects[i]
		}
	}
	return nil
}

// Save writes the config atomically (temp file + rename).
func (c *Config) Save(path string) error {
	sort.SliceStable(c.Projects, func(i, j int) bool { return c.Projects[i].Name < c.Projects[j].Name })
	var b bytes.Buffer
	b.WriteString("# taskmoor serve configuration (taskmoor project add / remove edit the projects).\n\n")
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // gone after the rename
	if _, err := tmp.Write(b.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LockConfig serializes read-modify-write edits of the configuration
// (project add and remove).
func LockConfig(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
