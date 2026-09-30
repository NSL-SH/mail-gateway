package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixedStore(path string, now *time.Time) *Store {
	s := &Store{path: path, now: func() time.Time { return *now }}
	s.st.Since = now.UTC()
	s.st.V = stateVersion
	return s
}

func TestRingIsCapped(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	s := fixedStore("", &now)
	for i := 0; i < RingSize+25; i++ {
		s.Record(Event{App: "app", To: "a@b.c", Status: StatusSent})
	}
	if got := len(s.st.Events); got != RingSize {
		t.Fatalf("ring holds %d events, want %d", got, RingSize)
	}
	sum := s.Summary("test")
	if len(sum.Recent) != RecentInStats {
		t.Fatalf("recent = %d, want %d", len(sum.Recent), RecentInStats)
	}
	if sum.Totals.H24.Sent != RingSize+25 {
		t.Fatalf("counters must not be capped by the ring: sent = %d", sum.Totals.H24.Sent)
	}
}

func TestRollUpWindowsAndPruning(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 30, 0, 0, time.UTC)
	s := fixedStore("", &now)
	rec := func(ago time.Duration, app string, st EventStatus, from string) {
		s.Record(Event{Time: now.Add(-ago), App: app, From: from, To: "x@y.z", Status: st})
	}
	rec(time.Hour, "vaultwarden", StatusSent, "vaultwarden.alice@nsl.sh")
	rec(2*time.Hour, "vaultwarden", StatusFailed, "")
	rec(3*time.Hour, "nextcloud", StatusSkipped, "nextcloud.alice@nsl.sh")
	rec(4*time.Hour, "nextcloud", StatusRateLimited, "")
	rec(3*24*time.Hour, "nextcloud", StatusSent, "nextcloud.alice@nsl.sh") // in 7d, not 24h
	rec(10*24*time.Hour, "immich", StatusSent, "immich.alice@nsl.sh")      // in 30d only
	rec(40*24*time.Hour, "ghost", StatusSent, "ghost.alice@nsl.sh")        // pruned at once

	sum := s.Summary("test")
	if want := (Counts{Sent: 1, Failed: 1, Skipped: 1, RateLimited: 1}); sum.Totals.H24 != want {
		t.Errorf("h24 = %+v, want %+v", sum.Totals.H24, want)
	}
	if want := (Counts{Sent: 2, Failed: 1, Skipped: 1, RateLimited: 1}); sum.Totals.D7 != want {
		t.Errorf("d7 = %+v, want %+v", sum.Totals.D7, want)
	}
	if len(sum.Apps) != 3 {
		t.Fatalf("apps = %+v, want vaultwarden, nextcloud, immich", sum.Apps)
	}
	if sum.Apps[0].App != "vaultwarden" || sum.Apps[1].App != "nextcloud" || sum.Apps[2].App != "immich" {
		t.Errorf("apps not ordered by last activity: %+v", sum.Apps)
	}
	nc := sum.Apps[1]
	if nc.Sent != 1 || nc.Skipped != 1 || nc.RateLimited != 1 || nc.From != "nextcloud.alice@nsl.sh" {
		t.Errorf("nextcloud = %+v", nc)
	}
	if sum.Apps[0].From != "vaultwarden.alice@nsl.sh" {
		t.Errorf("an event with no From must not blank the app's known sender: %+v", sum.Apps[0])
	}
	for _, b := range s.st.Buckets {
		if b.App == "ghost" {
			t.Errorf("bucket older than %d days was kept", RetentionDays)
		}
	}
	// Recent is in recording order, newest recorded first (the ring is not a
	// window: an old-but-just-recorded event stays until pushed out).
	if last := sum.Recent[len(sum.Recent)-1]; last.App != "vaultwarden" || last.Status != StatusSent {
		t.Errorf("oldest recent = %+v", last)
	}
}

func TestErrorIsTruncated(t *testing.T) {
	now := time.Now()
	s := fixedStore("", &now)
	long := make([]byte, 1000)
	for i := range long {
		long[i] = 'e'
	}
	s.Record(Event{App: "a", To: "b@c.d", Status: StatusFailed, Error: string(long)})
	if n := len(s.st.Events[0].Error); n != maxErrorLen {
		t.Fatalf("error kept %d chars, want %d", n, maxErrorLen)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	s := OpenStore(path)
	if !s.Persistent() {
		t.Fatal("store should be persistent in a writable dir")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("OpenStore must write the file straight away: %v", err)
	}
	s.Record(Event{App: "vaultwarden", From: "vaultwarden.alice@nsl.sh", To: "bob@example.com", Status: StatusSent})
	s.Record(Event{App: "vaultwarden", To: "carol@example.com", Status: StatusFailed, Error: "boom"})

	again := OpenStore(path)
	if len(again.st.Events) != 2 || !again.st.Since.Equal(s.st.Since) {
		t.Fatalf("reloaded state differs: %+v", again.st)
	}

	sum, err := statsFromFile(path, "1.1.0", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Persistent || sum.Totals.H24.Sent != 1 || sum.Totals.H24.Failed != 1 || sum.Version != "1.1.0" {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Recent[0].To != "carol@example.com" {
		t.Fatalf("recent must be newest first: %+v", sum.Recent)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("stray files next to stats.json: %v", entries)
	}
}

func TestStatsFromMissingFile(t *testing.T) {
	sum, err := statsFromFile(filepath.Join(t.TempDir(), "none.json"), "v", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Persistent || sum.Since != nil || sum.Apps == nil || sum.Recent == nil {
		t.Fatalf("missing file must give an empty, non-persistent summary with [] lists: %+v", sum)
	}
	b, _ := json.Marshal(sum)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"version", "since", "retentionDays", "totals", "apps", "recent", "persistent"} {
		if _, ok := m[k]; !ok {
			t.Errorf("summary JSON lacks %q: %s", k, b)
		}
	}
}

func TestUnwritablePathStaysInMemory(t *testing.T) {
	s := OpenStore(filepath.Join(t.TempDir(), "missing-dir", "stats.json"))
	if s.Persistent() {
		t.Fatal("store claims persistence without a writable directory")
	}
	s.Record(Event{App: "a", To: "b@c.d", Status: StatusSent})
	if s.Summary("v").Totals.H24.Sent != 1 {
		t.Fatal("in-memory store lost the event")
	}
}

func TestNilStoreRecordIsNoop(t *testing.T) {
	var s *Store
	s.Record(Event{App: "a"}) // must not panic
}
