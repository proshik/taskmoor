package serve

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJournalNumbersOldAndTornFeeds(t *testing.T) {
	j, err := newJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(j.dir, "s.jsonl")
	// a stage 1 feed: no numbers, two identical rows, then a torn line
	old := `{"at":"2026-11-22T10:00:00Z","type":"action","text":"x"}
{"at":"2026-11-22T10:00:00Z","type":"action","text":"x"}
{"at":"2026-11-22T10:00:01Z","type":"resp`
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	acts, _ := j.activities("s")
	if len(acts) != 2 || acts[0].Seq != 1 || acts[1].Seq != 2 {
		t.Fatalf("%+v", acts)
	}
	a, err := j.append("s", Activity{Type: "result", Text: "done"})
	if err != nil || a.Seq != 3 {
		t.Fatalf("%+v %v", a, err)
	}
	acts, _ = j.activities("s")
	if len(acts) != 3 || acts[2].Text != "done" || acts[2].Seq != 3 {
		t.Fatalf("the new record was joined to the torn line: %+v", acts)
	}
	// a fresh journal (a restart) continues from the highest number
	j2, _ := newJournal(filepath.Dir(j.dir))
	if a, _ := j2.append("s", Activity{Type: "x"}); a.Seq != 4 {
		t.Fatalf("seq %d", a.Seq)
	}
}
