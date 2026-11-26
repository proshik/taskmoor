package serve_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/serve"
)

// A client checking whether a service runs holds the store's lock for a
// moment; a service starting then waits it out. One that holds it for good
// is another service.
func TestOpenWaitsOutAMomentaryLockHolder(t *testing.T) {
	home := t.TempDir()
	paths := serve.Paths{Config: filepath.Join(home, "config.toml"), Store: filepath.Join(home, "store")}
	if err := os.MkdirAll(paths.Store, 0o700); err != nil {
		t.Fatal(err)
	}
	hold := func() *os.File {
		f, err := os.OpenFile(filepath.Join(paths.Store, "serve.lock"), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		return f
	}
	brief := hold()
	go func() { time.Sleep(200 * time.Millisecond); brief.Close() }()
	svc, err := serve.Open(paths, "test", nil, home)
	if err != nil {
		t.Fatalf("a momentary holder kept the service from starting: %v", err)
	}
	svc.Close()

	held := hold()
	defer held.Close()
	start := time.Now()
	if _, err := serve.Open(paths, "test", nil, home); err == nil || !strings.Contains(err.Error(), "another taskmoor serve is running") {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d < serve.StoreLockWait {
		t.Fatalf("gave up after %s", d)
	}
}
