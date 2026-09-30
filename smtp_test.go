package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-smtp"
)

const testMessage = "From: vaultwarden@local\r\nSubject: Hello\r\nContent-Type: text/plain\r\n\r\nBody\r\n"

// relayStub answers per recipient with the status in replies (default 200 sent).
func relayStub(t *testing.T, replies map[string]int, skipped bool) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/email/send" || r.Header.Get("Authorization") != "Bearer cred" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var req EmailRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got = append(got, req.To)
		mu.Unlock()
		code := replies[req.To]
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		if code == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "from": req.AppName + ".alice@nsl.sh", "skipped": skipped})
		} else {
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func session(url string, stats *Store, from string, to ...string) *SMTPSession {
	return &SMTPSession{backend: NewSMTPBackend(url, "cred", stats), from: from, to: to}
}

func memStore() *Store { return OpenStore("") }

func TestEveryRecipientGetsACopy(t *testing.T) {
	srv, got := relayStub(t, nil, false)
	st := memStore()
	s := session(srv.URL, st, "vaultwarden@local", "a@x.io", "<b@x.io>", "A@X.io", "c@x.io")
	if err := s.Data(strings.NewReader(testMessage)); err != nil {
		t.Fatalf("Data: %v", err)
	}
	if strings.Join(*got, ",") != "a@x.io,b@x.io,c@x.io" {
		t.Fatalf("relay calls = %v, want one per distinct recipient", *got)
	}
	sum := st.Summary("t")
	if sum.Totals.H24.Sent != 3 || sum.Apps[0].From != "vaultwarden.alice@nsl.sh" {
		t.Fatalf("stats = %+v", sum)
	}
}

func TestSkippedIsRecordedButAccepted(t *testing.T) {
	srv, _ := relayStub(t, nil, true)
	st := memStore()
	if err := session(srv.URL, st, "nextcloud@local", "a@x.io").Data(strings.NewReader(testMessage)); err != nil {
		t.Fatalf("a skipped send is still a 250 for the app: %v", err)
	}
	if c := st.Summary("t").Totals.H24; c.Skipped != 1 || c.Sent != 0 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestRateLimitIsTemporary(t *testing.T) {
	srv, _ := relayStub(t, map[string]int{"b@x.io": http.StatusTooManyRequests}, false)
	st := memStore()
	err := session(srv.URL, st, "app@local", "a@x.io", "b@x.io").Data(strings.NewReader(testMessage))
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 451 {
		t.Fatalf("err = %v, want SMTP 451", err)
	}
	if c := st.Summary("t").Totals.H24; c.Sent != 1 || c.RateLimited != 1 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestRelayRefusalIsPermanent(t *testing.T) {
	srv, _ := relayStub(t, map[string]int{"a@x.io": http.StatusInternalServerError, "b@x.io": http.StatusTooManyRequests}, false)
	st := memStore()
	err := session(srv.URL, st, "app@local", "a@x.io", "b@x.io").Data(strings.NewReader(testMessage))
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 554 {
		t.Fatalf("err = %v, want SMTP 554 (a refusal outranks a rate limit)", err)
	}
	sum := st.Summary("t")
	if sum.Totals.H24.Failed != 1 || sum.Totals.H24.RateLimited != 1 {
		t.Fatalf("counts = %+v", sum.Totals.H24)
	}
	if !strings.Contains(sum.Recent[1].Error, "500") {
		t.Fatalf("failed event lacks the relay's error: %+v", sum.Recent[1])
	}
}

func TestUnreachableRelayIsTemporary(t *testing.T) {
	srv, _ := relayStub(t, nil, false)
	url := srv.URL
	srv.Close()
	st := memStore()
	err := session(url, st, "app@local", "a@x.io").Data(strings.NewReader(testMessage))
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 451 {
		t.Fatalf("err = %v, want SMTP 451", err)
	}
	if c := st.Summary("t").Totals.H24; c.Failed != 1 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestNoRecipients(t *testing.T) {
	if err := session("http://unused", nil, "app@local").Data(strings.NewReader(testMessage)); err == nil {
		t.Fatal("expected an error with no recipients")
	}
}
