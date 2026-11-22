package serve_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proshik/taskmoor/internal/serve"
)

func TestPaths(t *testing.T) {
	env := map[string]string{"HOME": "/h"}
	get := func(k string) string { return env[k] }
	p, _ := serve.DefaultPaths(get)
	if p.Config != "/h/.config/taskmoor/config.toml" || p.Store != "/h/.local/state/taskmoor" {
		t.Fatalf("%+v", p)
	}
	env["XDG_CONFIG_HOME"], env["XDG_STATE_HOME"] = "/c", "/s"
	if p, _ = serve.DefaultPaths(get); p.Config != "/c/taskmoor/config.toml" || p.Store != "/s/taskmoor" {
		t.Fatalf("%+v", p)
	}
	env["TASKMOOR_HOME"] = "/t"
	if p, _ = serve.DefaultPaths(get); p.Config != "/t/config.toml" || p.Store != "/t/store" {
		t.Fatalf("%+v", p)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	missing, err := serve.LoadConfig(filepath.Join(dir, "none.toml"))
	if err != nil || missing.DefaultProfile != "claude" || missing.Port != serve.DefaultPort {
		t.Fatalf("%+v %v", missing, err)
	}
	for body, want := range map[string]string{
		"[[profiles]]\nname = \"x\"\nharness = \"gpt\"\n":      `harness "gpt"`,
		"[[profiles]]\nname = \"Bad\"\nharness = \"claude\"\n": "profile name",
		"default_profile = \"nope\"\n":                         "default_profile",
		"[[projects]]\nname = \"p\"\npath = \"rel\"\n":         "not absolute",
		"colour = 1\n": "unknown key",
		"[[projects]]\nname = \"p\"\npath = \"/a\"\n[[projects]]\nname = \"p\"\npath = \"/b\"\n": "registered twice",
	} {
		p := filepath.Join(dir, "c.toml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := serve.LoadConfig(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
}

func TestAnEmptyDomainListSurvivesASave(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	_ = os.WriteFile(p, []byte("[[profiles]]\nname = \"x\"\nharness = \"claude\"\nallowed_domains = []\n\n[[profiles]]\nname = \"y\"\nharness = \"claude\"\n"), 0o600)
	cfg, err := serve.LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(p); err != nil { // as `project add` does
		t.Fatal(err)
	}
	again, _ := serve.LoadConfig(p)
	if d := again.Profiles[0].AllowedDomains; d == nil || len(d) != 0 {
		t.Fatalf("x: %#v", d)
	}
	if d := again.Profiles[1].AllowedDomains; d != nil {
		t.Fatalf("y: %#v", d)
	}
}
