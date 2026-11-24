package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A secret replaces a readable file without ever being in it.
func TestWriteNewMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(p) // a reader of the old file never sees the secret
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := writeNew(p, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(old); string(b) != "old" {
		t.Fatalf("the old file now reads %q", b)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi.Mode(), err)
	}
	if data, _ := os.ReadFile(p); string(data) != "secret" {
		t.Fatal(string(data))
	}
}
