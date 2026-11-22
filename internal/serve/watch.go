package serve

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/proshik/taskmoor/internal/app"
)

// WatchInterval is how often the service looks for changes made outside
// it (merges, tags, the CLI's state writes, task files on main).
const WatchInterval = 2 * time.Second

// watch polls every registered project's fingerprint (the plan's cheap
// refresh check) and publishes "tasks" when one changes, so clients
// re-read the board.
func (s *Service) watch(ctx context.Context) {
	last := map[string]*app.Report{} // by name + "\x00" + path
	members := map[string]bool{}     // the registry as last seen
	var lastCfg string
	t := time.NewTicker(WatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		go s.fill(ctx) // "Keep N busy" (A4)
		data, _ := os.ReadFile(s.Paths.Config)
		cfg, err := s.Config()
		if err != nil {
			continue
		}
		// profiles, routing defaults or the registry changed: clients re-read
		if string(data) != lastCfg {
			if lastCfg != "" {
				s.bus.publish("tasks", map[string]string{"config": "changed"})
			}
			lastCfg = string(data)
		}
		seen := map[string]bool{}
		for _, p := range cfg.Projects {
			key := p.Name + "\x00" + p.Path
			seen[key] = true
			if !members[key] {
				members[key] = true
				s.bus.publish("tasks", map[string]string{"project": p.Name}) // registered, loadable or not
			}
			a := s.appFor(p)
			if rep := last[key]; rep != nil {
				if fp, err := a.Fingerprint(ctx, rep); err == nil && fp == rep.Fingerprint {
					continue
				}
			}
			next, err := a.Report(ctx)
			if err != nil {
				continue
			}
			_, had := last[key]
			last[key] = next
			if had {
				s.bus.publish("tasks", map[string]string{"project": p.Name})
			}
		}
		for key := range members {
			if !seen[key] {
				delete(members, key)
				delete(last, key)
				name, _, _ := strings.Cut(key, "\x00")
				s.bus.publish("tasks", map[string]string{"project": name, "removed": "true"})
			}
		}
	}
}
