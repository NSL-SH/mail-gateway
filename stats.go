package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Delivery statistics.
//
// The gateway keeps a small record of what it relayed so the owner can see it
// (mesh-console's Email page reads it through `mail-gateway stats`, run with
// docker exec — there is no stats port on the pcs network). Only envelope
// metadata is kept: never a subject or a body.
//
// Two structures, persisted together as one JSON file:
//   - a ring of the most recent events (who sent what to whom, and the outcome);
//   - per-app hourly counters, kept for RetentionDays, which the 24h / 7d
//     totals and the per-app table are computed from. Hourly rather than daily
//     so a "last 24 hours" total is exact, not a calendar-day approximation.

const (
	RingSize      = 200
	RetentionDays = 30
	RecentInStats = 20
	maxErrorLen   = 200
	stateVersion  = 1
)

type EventStatus string

const (
	StatusSent        EventStatus = "sent"
	StatusSkipped     EventStatus = "skipped"      // relay accepted it but has no delivery provider configured
	StatusFailed      EventStatus = "failed"       // relay unreachable or refused it
	StatusRateLimited EventStatus = "rate_limited" // relay answered 429
)

type Event struct {
	Time   time.Time   `json:"time"`
	App    string      `json:"app"`
	From   string      `json:"from,omitempty"` // the sender the relay enforced; empty when it never answered
	To     string      `json:"to"`
	Status EventStatus `json:"status"`
	Error  string      `json:"error,omitempty"`
}

type Counts struct {
	Sent        int `json:"sent"`
	Failed      int `json:"failed"`
	Skipped     int `json:"skipped"`
	RateLimited int `json:"rateLimited"`
}

func (c *Counts) add(s EventStatus, n int) {
	switch s {
	case StatusSent:
		c.Sent += n
	case StatusSkipped:
		c.Skipped += n
	case StatusRateLimited:
		c.RateLimited += n
	default:
		c.Failed += n
	}
}

func (c *Counts) merge(o Counts) {
	c.Sent += o.Sent
	c.Failed += o.Failed
	c.Skipped += o.Skipped
	c.RateLimited += o.RateLimited
}

// Bucket counts one app's events in one UTC hour.
type Bucket struct {
	Hour time.Time `json:"hour"`
	App  string    `json:"app"`
	From string    `json:"from,omitempty"`
	Last time.Time `json:"last"`
	Counts
}

type state struct {
	V       int       `json:"v"`
	Since   time.Time `json:"since"`
	Events  []Event   `json:"events"`
	Buckets []Bucket  `json:"buckets"`
}

// Store is safe for concurrent use. A Store with an empty path, or whose file
// cannot be written, keeps working in memory.
type Store struct {
	mu         sync.Mutex
	path       string
	st         state
	persistent bool
	warned     bool
	now        func() time.Time
}

// OpenStore loads path if it exists and writes it back straight away, so the
// file's presence tells a reader that persistence works.
func OpenStore(path string) *Store {
	s := &Store{path: path, now: time.Now}
	if path != "" {
		if st, err := readState(path); err == nil {
			s.st = st
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("stats: ignoring unreadable %s: %v", path, err)
		}
	}
	if s.st.Since.IsZero() {
		s.st.Since = s.now().UTC()
	}
	s.st.V = stateVersion
	s.mu.Lock()
	s.saveLocked()
	s.mu.Unlock()
	return s
}

func (s *Store) Persistent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistent
}

// Record adds one delivery attempt (one recipient) and persists.
func (s *Store) Record(e Event) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if e.Time.IsZero() {
		e.Time = now
	}
	e.Time = e.Time.UTC()
	if len(e.Error) > maxErrorLen {
		e.Error = e.Error[:maxErrorLen]
	}

	s.st.Events = append(s.st.Events, e)
	if n := len(s.st.Events); n > RingSize {
		s.st.Events = append([]Event(nil), s.st.Events[n-RingSize:]...)
	}

	hour := e.Time.Truncate(time.Hour)
	var b *Bucket
	for i := range s.st.Buckets {
		if s.st.Buckets[i].App == e.App && s.st.Buckets[i].Hour.Equal(hour) {
			b = &s.st.Buckets[i]
			break
		}
	}
	if b == nil {
		s.st.Buckets = append(s.st.Buckets, Bucket{Hour: hour, App: e.App})
		b = &s.st.Buckets[len(s.st.Buckets)-1]
	}
	b.add(e.Status, 1)
	if e.From != "" {
		b.From = e.From
	}
	if e.Time.After(b.Last) {
		b.Last = e.Time
	}
	s.pruneLocked(now)
	s.saveLocked()
}

func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.Add(-RetentionDays * 24 * time.Hour)
	kept := s.st.Buckets[:0]
	for _, b := range s.st.Buckets {
		if !b.Hour.Before(cutoff.Truncate(time.Hour)) {
			kept = append(kept, b)
		}
	}
	s.st.Buckets = kept
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	if err := writeState(s.path, s.st); err != nil {
		s.persistent = false
		if !s.warned {
			log.Printf("stats: cannot write %s, keeping statistics in memory only: %v", s.path, err)
			s.warned = true
		}
		return
	}
	s.persistent = true
}

// Summary returns what `mail-gateway stats` prints for this store's state.
func (s *Store) Summary(version string) Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return summarize(s.st, s.now().UTC(), version, s.persistent)
}

func readState(path string) (state, error) {
	var st state
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

// writeState replaces path atomically: a reader (the stats subcommand, run in
// the same container) never sees a half-written file.
func writeState(path string, st state) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".stats-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ---- summary ---------------------------------------------------------------

type AppSummary struct {
	App  string    `json:"app"`
	From string    `json:"from,omitempty"`
	Last time.Time `json:"last"`
	Counts
}

type Summary struct {
	Version       string     `json:"version"`
	Since         *time.Time `json:"since"` // start of the record; nil when there is none
	RetentionDays int        `json:"retentionDays"`
	Totals        struct {
		H24 Counts `json:"h24"`
		D7  Counts `json:"d7"`
	} `json:"totals"`
	Apps       []AppSummary `json:"apps"`   // over the whole retention window, most recently active first
	Recent     []Event      `json:"recent"` // newest first
	Persistent bool         `json:"persistent"`
}

func summarize(st state, now time.Time, version string, persistent bool) Summary {
	out := Summary{Version: version, RetentionDays: RetentionDays, Persistent: persistent, Apps: []AppSummary{}, Recent: []Event{}}
	if !st.Since.IsZero() {
		since := st.Since
		out.Since = &since
	}
	// An hour bucket counts toward a window when any part of it falls inside,
	// so "24h" can include up to 59 extra minutes. Good enough for a dashboard.
	h24 := now.Add(-24 * time.Hour).Truncate(time.Hour)
	d7 := now.Add(-7 * 24 * time.Hour).Truncate(time.Hour)
	apps := map[string]*AppSummary{}
	for _, b := range st.Buckets {
		if !b.Hour.Before(h24) {
			out.Totals.H24.merge(b.Counts)
		}
		if !b.Hour.Before(d7) {
			out.Totals.D7.merge(b.Counts)
		}
		a := apps[b.App]
		if a == nil {
			a = &AppSummary{App: b.App}
			apps[b.App] = a
		}
		a.merge(b.Counts)
		if b.Last.After(a.Last) {
			a.Last = b.Last
			if b.From != "" {
				a.From = b.From
			}
		}
		if a.From == "" {
			a.From = b.From
		}
	}
	for _, a := range apps {
		out.Apps = append(out.Apps, *a)
	}
	sort.Slice(out.Apps, func(i, j int) bool {
		if !out.Apps[i].Last.Equal(out.Apps[j].Last) {
			return out.Apps[i].Last.After(out.Apps[j].Last)
		}
		return out.Apps[i].App < out.Apps[j].App
	})
	for i := len(st.Events) - 1; i >= 0 && len(out.Recent) < RecentInStats; i-- {
		out.Recent = append(out.Recent, st.Events[i])
	}
	return out
}

// statsFromFile is the `mail-gateway stats` subcommand's view: it reads the
// file the running server writes, so it works from a docker exec without a
// port. A missing file means the server could not persist (or never started):
// an empty summary with persistent=false, not an error.
func statsFromFile(path, version string, now time.Time) (Summary, error) {
	st, err := readState(path)
	if errors.Is(err, os.ErrNotExist) {
		return summarize(state{}, now, version, false), nil
	}
	if err != nil {
		return Summary{}, err
	}
	return summarize(st, now.UTC(), version, true), nil
}
