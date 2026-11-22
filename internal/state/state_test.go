package state_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/schema"
	"github.com/proshik/taskmoor/internal/state"
)

func file(id string) *state.File {
	return &state.File{Format: 1, ID: id, Lifecycle: state.InProgress, Worktree: "/w", Branch: "b",
		Base: "0123456789abcdef", StartedAt: "2026-11-22T09:30:00Z", UpdatedAt: "2026-11-22T09:30:00Z", Writer: "taskmoor test"}
}

func TestCreateIsCreateIfAbsent(t *testing.T) {
	sch, err := schema.Load()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, exists := 0, 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := state.Create(dir, file("TM-001"), sch)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, state.ErrExists):
				exists++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || exists != 15 {
		t.Fatalf("wins %d, exists %d", wins, exists)
	}
	des, _ := os.ReadDir(dir)
	for _, de := range des {
		if de.Name() != "TM-001.json" {
			t.Errorf("left behind: %s", de.Name())
		}
	}
}

func TestEncodeRefusesSchemaInvalid(t *testing.T) {
	sch, _ := schema.Load()
	f := file("TM-001")
	f.Worktree = "relative"
	err := state.Create(t.TempDir(), f, sch)
	if err == nil || !strings.Contains(err.Error(), "fails state.schema.json") {
		t.Fatalf("err %v", err)
	}
	f = file("TM-001")
	f.Harness = &state.Harness{Name: "claude", Status: "working", Kind: "permission", ObservedAt: f.StartedAt}
	if err := state.Create(t.TempDir(), f, sch); err == nil {
		t.Fatal("kind without blocked accepted")
	}
}

func TestReplaceKeepsOneWholeFile(t *testing.T) {
	sch, _ := schema.Load()
	dir := t.TempDir()
	if err := state.Create(dir, file("TM-001"), sch); err != nil {
		t.Fatal(err)
	}
	f := file("TM-001")
	f.Lifecycle = state.Review
	if err := state.Replace(dir, f, sch); err != nil {
		t.Fatal(err)
	}
	e, ok, err := state.Read(dir, "TM-001", sch)
	if err != nil || !ok || !e.Valid() || e.File.Lifecycle != state.Review {
		t.Fatalf("read %+v %v %v", e, ok, err)
	}
	des, _ := os.ReadDir(dir)
	if len(des) != 1 {
		t.Fatalf("%d files in the state directory, want 1", len(des))
	}
}

func TestReplaceReadersSeeWholeVersions(t *testing.T) {
	sch, _ := schema.Load()
	dir := t.TempDir()
	if err := state.Create(dir, file("TM-001"), sch); err != nil {
		t.Fatal(err)
	}
	checked := false
	restore := state.SetBeforePublish(func() {
		// the new version is complete in its temporary file; the target is
		// still the whole previous version
		e, ok, err := state.Read(dir, "TM-001", sch)
		if err != nil || !ok || !e.Valid() || e.File.Lifecycle != state.InProgress {
			t.Errorf("before publish: %+v %v %v", e, ok, err)
		}
		checked = true
	})
	defer restore()
	f := file("TM-001")
	f.Lifecycle = state.Review
	if err := state.Replace(dir, f, sch); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("hook not run")
	}
	if e, _, _ := state.Read(dir, "TM-001", sch); !e.Valid() || e.File.Lifecycle != state.Review {
		t.Fatalf("after publish: %+v", e)
	}
}

func TestDirectorySyncFailureIsReportedAsNotDurable(t *testing.T) {
	sch, _ := schema.Load()
	dir := t.TempDir()
	restore := state.SetSyncDir(func(string) error { return errors.New("EIO") })
	defer restore()
	err := state.Create(dir, file("TM-001"), sch)
	var nd *state.NotDurableError
	if !errors.As(err, &nd) {
		t.Fatalf("err %v, want NotDurableError", err)
	}
	if _, ok, _ := state.Read(dir, "TM-001", sch); !ok {
		t.Fatal("the file is not published")
	}
	if err := state.Remove(dir, "TM-001"); !errors.As(err, &nd) {
		t.Fatalf("remove: %v", err)
	}
}

func TestIntegralNumbersInStateFile(t *testing.T) {
	sch, _ := schema.Load()
	dir := t.TempDir()
	data := `{"format": 1.0, "id": "TM-001", "lifecycle": "in_progress", "worktree": "/w", "branch": "b",
	"base": "0123456", "started_at": "2026-11-08T10:00:00Z", "updated_at": "2026-11-08T10:00:00Z", "writer": "x",
	"harness": {"name": "claude", "status": "exited", "exit_code": 2e0, "observed_at": "2026-11-08T10:00:00Z"}}`
	if err := os.WriteFile(dir+"/TM-001.json", []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	e, _, _ := state.Read(dir, "TM-001", sch)
	if !e.Valid() || e.File.Format != 1 || *e.File.Harness.ExitCode != 2 {
		t.Fatalf("%+v", e)
	}
}

func TestFIFOIsReportedNotRead(t *testing.T) {
	sch, _ := schema.Load()
	dir := t.TempDir()
	if err := syscall.Mkfifo(dir+"/TM-001.json", 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan []state.Entry, 1)
	go func() {
		es, _ := state.ReadAll(dir, sch)
		done <- es
	}()
	select {
	case es := <-done:
		if len(es) != 1 || es[0].Valid() || !strings.Contains(es[0].Problem, "not a regular file") {
			t.Fatalf("%+v", es)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
	if err := state.Remove(dir, "TM-001"); err != nil {
		t.Fatal(err)
	}
}

func TestOversizedStateFile(t *testing.T) {
	sch, _ := schema.Load()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/TM-001.json", make([]byte, state.MaxSize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if e, _, _ := state.Read(dir, "TM-001", sch); e.Valid() || !strings.Contains(e.Problem, "more than") {
		t.Fatalf("%+v", e)
	}
}

func TestLockWaitEndsWithContext(t *testing.T) {
	dir := t.TempDir()
	unlock, err := state.Lock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := state.Lock(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the wait did not end with the context")
	}
}

func TestReplaceUnderConcurrentReads(t *testing.T) {
	sch, _ := schema.Load()
	dir := t.TempDir()
	if err := state.Create(dir, file("TM-001"), sch); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	bad := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stop:
				close(bad)
				return
			default:
			}
			e, ok, err := state.Read(dir, "TM-001", sch)
			if err != nil || !ok || !e.Valid() {
				bad <- fmt.Sprintf("a reader saw ok=%v err=%v problem=%q", ok, err, e.Problem)
				return
			}
		}
	}()
	for i := 0; i < 300; i++ {
		f := file("TM-001")
		if i%2 == 0 {
			f.Lifecycle = state.Review
		}
		if err := state.Replace(dir, f, sch); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	if msg, ok := <-bad; ok {
		t.Fatal(msg)
	}
}
