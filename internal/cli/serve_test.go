package cli_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/cli"
	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

// runIn runs the CLI with TASKMOOR_HOME set, like a person's shell would.
func runIn(t *testing.T, home, dir string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	getenv := func(k string) string {
		if k == "TASKMOOR_HOME" {
			return home
		}
		return os.Getenv(k)
	}
	code := cli.Run(context.Background(), args, cli.Env{Stdout: &out, Stderr: &errb, Dir: dir, Version: "0.1.0-test",
		Getenv: getenv, Now: time.Now})
	return result{code, out.String(), errb.String()}
}

func TestServiceThroughTheCLI(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001", Title: "Fix the thing"})
	home := t.TempDir()
	fake := filepath.Join(t.TempDir(), "claude")
	if out, err := exec.Command("go", "build", "-o", fake, "../serve/testdata/fakeclaude").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Setenv("FAKE_SCENARIO", "work")
	t.Setenv("FAKE_LOG", filepath.Join(home, "fake.log"))
	cfg := &serve.Config{Port: 0, DefaultProfile: "fake", Profiles: []serve.Profile{{Name: "fake", Harness: "claude", Command: fake}}}
	ln := freeTCPPort(t)
	cfg.Port = ln
	if err := cfg.Save(filepath.Join(home, "config.toml")); err != nil {
		t.Fatal(err)
	}

	if res := runIn(t, home, r.Dir, "project", "add", ".", "--name", "Bad Name"); res.code != 2 {
		t.Fatalf("project add with a bad name: %d %s", res.code, res.stderr)
	}
	if res := runIn(t, home, r.Dir, "project", "add", "."); res.code != 0 || !strings.Contains(res.stdout, "project repo: "+r.Dir) {
		t.Fatalf("project add: %d %s%s", res.code, res.stdout, res.stderr)
	}
	if res := runIn(t, home, r.Dir, "project", "list"); res.code != 0 || !strings.Contains(res.stdout, "repo") {
		t.Fatalf("project list after a refused add: %d %s%s", res.code, res.stdout, res.stderr)
	}
	if res := runIn(t, home, r.Dir, "run", "TM-001"); res.code != 2 || !strings.Contains(res.stderr, "not running") {
		t.Fatalf("run without a service: %d %s", res.code, res.stderr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan result, 1)
	go func() {
		var out, errb bytes.Buffer
		getenv := func(k string) string {
			if k == "TASKMOOR_HOME" {
				return home
			}
			return os.Getenv(k)
		}
		code := cli.Run(ctx, []string{"serve"}, cli.Env{Stdout: &out, Stderr: &errb, Dir: r.Dir, Version: "0.1.0-test", Getenv: getenv, Now: time.Now})
		served <- result{code, out.String(), errb.String()}
	}()
	defer func() {
		cancel()
		if res := <-served; res.code != 0 {
			t.Errorf("serve: %d %s", res.code, res.stderr)
		}
	}()
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wcancel()
	if _, err := serve.WaitReady(wctx, filepath.Join(home, "store")); err != nil {
		t.Fatal(err)
	}

	res := runIn(t, home, r.Dir, "run", "TM-001")
	if res.code != 0 || !strings.Contains(res.stdout, "TM-001 started: session") {
		t.Fatalf("run: %d %s%s", res.code, res.stdout, res.stderr)
	}
	// the task is the service's now: the CLI may not review it (A6)
	if res := runIn(t, home, r.Dir, "review", "TM-001"); res.code != 1 || !strings.Contains(res.stderr, "owned by a running taskmoor serve") {
		t.Fatalf("review of a service task: %d %s", res.code, res.stderr)
	}
	var inbox result
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if inbox = runIn(t, home, r.Dir, "inbox"); strings.Contains(inbox.stdout, "Which approach?") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(inbox.stdout, "* 1) Small fix — keep it small") || !strings.Contains(inbox.stdout, "  2) Rewrite") {
		t.Fatalf("inbox:\n%s", inbox.stdout)
	}
	id := regexp.MustCompile(`(?m)^([0-9a-f]{12})  TM-001  question`).FindStringSubmatch(inbox.stdout)
	if id == nil {
		t.Fatalf("no ask id in:\n%s", inbox.stdout)
	}
	if res := runIn(t, home, r.Dir, "answer", id[1], "3"); res.code != 1 || !strings.Contains(res.stderr, "not an option") {
		t.Fatalf("bad answer: %d %s", res.code, res.stderr)
	}
	if res := runIn(t, home, r.Dir, "answer", id[1], "1"); res.code != 0 {
		t.Fatalf("answer: %d %s", res.code, res.stderr)
	}
	feed := runIn(t, home, r.Dir, "feed", "TM-001", "--follow")
	if feed.code != 0 || !strings.Contains(feed.stdout, "Going with Small fix (Recommended).") || !strings.Contains(feed.stdout, "— session complete") && !strings.Contains(feed.stdout, "result       Done: small fix committed.") {
		t.Fatalf("feed: %d\n%s%s", feed.code, feed.stdout, feed.stderr)
	}
	if res := runIn(t, home, r.Dir, "sessions"); !strings.Contains(res.stdout, "TM-001") || !strings.Contains(res.stdout, "subscription") {
		t.Fatalf("sessions: %s", res.stdout)
	}
	if res := runIn(t, home, r.Dir, "say", "--json", "TM-001", "also", "this"); res.code != 0 || !strings.Contains(res.stdout, `"format": 1`) {
		t.Fatalf("say --json: %d %s%s", res.code, res.stdout, res.stderr)
	}
	if res := runIn(t, home, r.Dir, "feed", "TM-001", "--follow", "--json"); res.code != 0 || !strings.Contains(res.stdout, `"ended":"complete"`) {
		t.Fatalf("feed --follow --json: %d %s%s", res.code, res.stdout, res.stderr)
	}
	// release goes through the service that owns the task
	if res := runIn(t, home, r.Dir, "release", "TM-001"); res.code != 0 || !strings.Contains(res.stdout, "released through taskmoor serve") {
		t.Fatalf("release: %d %s%s", res.code, res.stdout, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(r.StateDir(), "TM-001.json")); !os.IsNotExist(err) {
		t.Fatal("state file left after release")
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := netListen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func netListen() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
