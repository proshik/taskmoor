package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Claude session's sandbox settings sit beside its record as
// <id>.settings.json; neither they nor a stray JSON file are sessions.
func TestJournalLoadsOnlySessionRecords(t *testing.T) {
	j, err := newJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := newUUID()
	if err := j.saveSession(&Session{ID: id, Task: "TM-001", Profile: "fake", State: StateStopped}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.append(id, Activity{Type: ActPrompt, Text: "go"}); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		id + ".settings.json":      `{"sandbox":{"enabled":true},"permissions":{"deny":["Read(/x)"]}}`,
		"notes.json":               `{"hello":"world"}`,
		"other.json":               `{"id":"` + newUUID() + `","task":"TM-002"}`,
		"broken.json":              `{"id":`,
		"." + id + ".json.123.tmp": `{"id":"` + id + `"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(j.dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, warnings, err := j.sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Task != "TM-001" {
		t.Fatalf("loaded %d sessions, want only %s: %+v", len(got), id, got)
	}
	// the files that are not their name's record are named in the log; the
	// settings file and the temporary write are not records at all
	joined := strings.Join(warnings, "\n")
	for _, name := range []string{"notes.json", "other.json", "broken.json"} {
		if !strings.Contains(joined, "sessions/"+name) {
			t.Errorf("no warning for %s in:\n%s", name, joined)
		}
	}
	if len(warnings) != 3 {
		t.Errorf("want 3 warnings, got:\n%s", joined)
	}
	// skipped files stay on disk
	for name := range files {
		if _, err := os.Stat(filepath.Join(j.dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

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
