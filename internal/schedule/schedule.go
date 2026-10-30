// Package schedule keeps reminders and fires them when due. It runs inside the
// daemon rather than as systemd timers or launchd jobs: delivery needs the
// daemon and the host anyway, and one implementation works on every OS.
package schedule

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/metalmatze/kelpie/internal/core"
)

// maxSleep bounds every wait. Go timers run on the monotonic clock, which on
// Linux stops while the machine sleeps, so the scheduler wakes up at least
// this often and compares wall-clock time instead of trusting the timer.
const maxSleep = 30 * time.Second

// Fire receives a due reminder and how late it fires: after sleep or a
// restart that can be hours.
type Fire func(r core.Reminder, late time.Duration)

type Scheduler struct {
	path    string
	catchUp time.Duration
	fire    Fire
	now     func() time.Time

	mu        sync.Mutex
	reminders map[string]core.Reminder
	nextID    int
	wake      chan struct{}
}

// New loads the reminders saved at path, if any. Reminders older than
// catchUp when they come due are dropped instead of fired.
func New(path string, catchUp time.Duration, fire Fire) (*Scheduler, error) {
	s := &Scheduler{
		path:      path,
		catchUp:   catchUp,
		fire:      fire,
		now:       time.Now,
		reminders: map[string]core.Reminder{},
		wake:      make(chan struct{}, 1),
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []core.Reminder
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, r := range list {
		s.reminders[r.ID] = r
		if n, err := strconv.Atoi(strings.TrimPrefix(r.ID, "r")); err == nil && n > s.nextID {
			s.nextID = n
		}
	}
	return s, nil
}

// ParseCron accepts five-field cron expressions, descriptors like @daily and
// @every 1h, and a CRON_TZ= prefix. Times are local unless the prefix says
// otherwise.
func ParseCron(expr string) (cron.Schedule, error) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("cron %q: %w", expr, err)
	}
	return sched, nil
}

// Add stores r with a new ID. A cron reminder's Due is computed here; a
// one-shot reminder must carry its own.
func (s *Scheduler) Add(r core.Reminder) (core.Reminder, error) {
	now := s.now()
	if r.Cron != "" {
		sched, err := ParseCron(r.Cron)
		if err != nil {
			return core.Reminder{}, err
		}
		r.Due = sched.Next(now)
	}
	if r.Due.IsZero() {
		return core.Reminder{}, errors.New("reminder needs a due time or a cron schedule")
	}
	if strings.TrimSpace(r.Text) == "" {
		return core.Reminder{}, errors.New("reminder needs a text")
	}
	// Round(0) drops the monotonic reading, so comparisons use the wall
	// clock, the same as for reminders loaded from disk.
	r.Due = r.Due.Round(0)
	r.Created = now.Round(0)
	s.mu.Lock()
	s.nextID++
	r.ID = fmt.Sprintf("r%d", s.nextID)
	s.reminders[r.ID] = r
	s.save()
	s.mu.Unlock()
	s.poke()
	return r, nil
}

func (s *Scheduler) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reminders[id]; !ok {
		return fmt.Errorf("no reminder %s", id)
	}
	delete(s.reminders, id)
	s.save()
	return nil
}

// List returns the reminders of one workspace, or all with workspace empty,
// soonest first.
func (s *Scheduler) List(workspace string) []core.Reminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.Reminder
	for _, r := range s.reminders {
		if workspace == "" || r.Workspace == workspace {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, compare)
	return out
}

// Prune ends the reminders of workspaces that no longer exist, as their
// watches end.
func (s *Scheduler) Prune(alive func(workspace string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id, r := range s.reminders {
		if !alive(r.Workspace) {
			slog.Info("reminder dropped: workspace closed", "id", id, "workspace", r.Workspace)
			delete(s.reminders, id)
			changed = true
		}
	}
	if changed {
		s.save()
	}
}

// Move re-keys the reminders of a workspace whose ID changed.
func (s *Scheduler) Move(from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id, r := range s.reminders {
		if r.Workspace == from {
			r.Workspace = to
			s.reminders[id] = r
			changed = true
		}
	}
	if changed {
		s.save()
	}
}

// Run fires reminders until ctx ends.
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		wait := s.Tick()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-s.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// Tick fires every due reminder and returns how long to sleep until the next
// check. A one-shot reminder ends after firing; a cron reminder moves to its
// next time after now, so a long sleep fires it once, not once per missed run.
func (s *Scheduler) Tick() time.Duration {
	now := s.now().Round(0)
	type due struct {
		r    core.Reminder
		late time.Duration
	}
	var fire []due
	s.mu.Lock()
	changed := false
	for id, r := range s.reminders {
		if r.Due.After(now) {
			continue
		}
		changed = true
		late := now.Sub(r.Due)
		if late > s.catchUp {
			slog.Warn("reminder skipped: missed by more than catch_up", "id", id, "workspace", r.Workspace, "due", r.Due, "catch_up", s.catchUp)
		} else {
			fire = append(fire, due{r, late})
		}
		if r.Cron == "" {
			delete(s.reminders, id)
			continue
		}
		sched, err := ParseCron(r.Cron)
		if err != nil { // validated on Add; only a hand-edited file gets here
			slog.Error("reminder dropped", "id", id, "err", err)
			delete(s.reminders, id)
			continue
		}
		r.Due = sched.Next(now)
		s.reminders[id] = r
	}
	if changed {
		s.save()
	}
	wait := maxSleep
	for _, r := range s.reminders {
		wait = min(wait, r.Due.Sub(now))
	}
	s.mu.Unlock()

	slices.SortFunc(fire, func(a, b due) int { return compare(a.r, b.r) })
	for _, f := range fire {
		slog.Info("reminder due", "id", f.r.ID, "workspace", f.r.Workspace, "late", f.late.Round(time.Second))
		s.fire(f.r, f.late)
	}
	return max(wait, 0)
}

func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// save must be called with s.mu held.
func (s *Scheduler) save() {
	if s.path == "" {
		return
	}
	list := make([]core.Reminder, 0, len(s.reminders))
	for _, r := range s.reminders {
		list = append(list, r)
	}
	slices.SortFunc(list, compare)
	data, err := json.MarshalIndent(list, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(s.path), 0o755)
	}
	if err == nil {
		// Write and rename, so a crash mid-write cannot lose every reminder.
		tmp := s.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, s.path)
		}
	}
	if err != nil {
		slog.Error("saving reminders failed", "path", s.path, "err", err)
	}
}

func compare(a, b core.Reminder) int {
	return cmp.Or(a.Due.Compare(b.Due), cmp.Compare(a.ID, b.ID))
}
