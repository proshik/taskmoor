// Package validate runs every check of the task format over a snapshot and
// returns the findings.
package validate

import (
	"fmt"
	"strings"

	"github.com/proshik/taskmoor/internal/derive"
	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/project"
)

// Run checks the snapshot. statuses is derive.Derive's result for it.
func Run(s *project.Snapshot, statuses map[string]*derive.Status) []finding.Finding {
	fs := append([]finding.Finding(nil), s.Findings...)
	cfg := s.Config

	for _, t := range s.Tasks {
		for i, dep := range t.DependsOn {
			line := t.Line(fmt.Sprintf("/depends_on/%d", i))
			switch {
			case dep == t.ID:
				fs = append(fs, finding.Errorf(t.File, line, "depends_on names the task itself"))
			case s.Task(dep) == nil:
				fs = append(fs, finding.Errorf(t.File, line, "depends_on %s: no such task", dep))
			}
		}
		if s.ConfigBroken { // the checks below need tasks.toml
			continue
		}
		if !strings.HasPrefix(t.ID, cfg.Project.IDPrefix+"-") {
			fs = append(fs, finding.Errorf(t.File, t.Line("/id"), "id %s does not use the project's prefix %s", t.ID, cfg.Project.IDPrefix))
		}
		if _, ok := cfg.Components[t.Component]; !ok {
			fs = append(fs, finding.Errorf(t.File, t.Line("/component"), "component %s is not defined in %s", t.Component, project.ConfigFile))
		}
		if t.AfterRelease != "" && s.MainHead != "" {
			ar := s.AfterRelease[t.ID]
			line := t.Line("/after_release")
			switch {
			case ar.Commit == "":
				fs = append(fs, finding.Errorf(t.File, line, "after_release %s names no commit", t.AfterRelease))
			case !ar.OnMain:
				fs = append(fs, finding.Errorf(t.File, line, "after_release %s is not on the main branch %s", t.AfterRelease, cfg.Project.MainBranch))
			}
		}
		if s.Source == project.SourceWorktree && s.OnMain != nil {
			name := t.File[len(project.Dir)+1:]
			if !s.OnMain[name] {
				fs = append(fs, finding.Warnf(t.File, 0, "not on the main branch %s yet", cfg.Project.MainBranch))
			}
		}
	}
	for _, cyc := range derive.Cycles(s.Tasks) {
		t := s.Task(cyc[0])
		fs = append(fs, finding.Errorf(t.File, t.Line("/depends_on"), "dependency cycle: %s", strings.Join(cyc, " → ")))
	}

	for _, e := range s.States {
		switch {
		case e.File == nil || e.Problem != "":
			fs = append(fs, e.Finding(finding.Error))
		case s.Task(e.Name) == nil:
			fs = append(fs, finding.Errorf(e.Path, 0, "state file for %s, which is not a known task", e.Name))
		case statuses[e.Name] != nil && statuses[e.Name].Stale:
			fs = append(fs, finding.Warnf(e.Path, 0, "stale: %s is %s, the state file still says %s", e.Name, statuses[e.Name].Lifecycle, e.File.Lifecycle))
		}
	}

	for id, commits := range s.AllMerges {
		if len(commits) > 1 {
			fs = append(fs, finding.Errorf("", 0, "%s has %d `Task:` commits on %s's first-parent history (%s); the oldest, %s, counts",
				id, len(commits), cfg.Project.MainBranch, shorts(commits), short(commits[len(commits)-1])))
		}
		if s.Task(id) == nil {
			fs = append(fs, finding.Warnf("", 0, "commit %s has `Task: %s`, which is not a known task", short(commits[0]), id))
		}
	}
	for _, tr := range s.BadTrailers {
		fs = append(fs, finding.Warnf("", 0, "commit %s has `Task: %s`, which is not a %s task id; ignored", short(tr.Commit), tr.Value, cfg.Project.IDPrefix))
	}
	finding.Sort(fs)
	return fs
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func shorts(hs []string) string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = short(h)
	}
	return strings.Join(out, ", ")
}
