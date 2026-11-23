package server

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLiveStreamSaysWhenAnIncidentOpens(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(h.session)
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/live", nil)
	req = asPerson(req, admin, "Admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	got := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			got <- sc.Text()
		}
		close(got)
	}()
	h.openByHand(t, "Checkout is failing", "critical", "")
	deadline := time.After(livePoll + 3*time.Second)
	for {
		select {
		case line, ok := <-got:
			if !ok {
				t.Fatal("stream closed before a change")
			}
			if line == "event: change" {
				return
			}
		case <-deadline:
			t.Fatal("no change event after an incident opened")
		}
	}
}

func TestLiveStreamNeedsTheFrontDoor(t *testing.T) {
	h := newHarness(t)
	rec := h.do(h.session, bearer(httptest.NewRequest(http.MethodGet, "/live", nil), shared))
	if rec.Code == http.StatusOK {
		t.Fatalf("a bearer token opened the live stream: %d", rec.Code)
	}
}

func TestPeopleShowAsNamesNeverAddresses(t *testing.T) {
	directory.Store(&map[string]string{"pat@example.com": "Pat Doe"})
	defer directory.Store(nil)
	for in, want := range map[string]string{
		"pat@example.com":      "Pat Doe",
		"PAT@example.com":      "Pat Doe",
		"sam.lee7@example.com": "Sam Lee",
		"alertmanager":         "alertmanager",
		"":                     "nobody",
	} {
		if got := personName(in); got != want {
			t.Errorf("personName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := initials("pat@example.com"); got != "PD" {
		t.Errorf("initials = %q", got)
	}
	if got := named("Resolved by sam.lee7@example.com after pat@example.com looked"); got != "Resolved by Sam Lee after Pat Doe looked" {
		t.Errorf("named = %q", got)
	}
}

func TestWholeKeepsDatesOnOneLine(t *testing.T) {
	got := string(whole("watchdog has not checked in since 2026-11-08 22:05 UTC & <now>"))
	want := `watchdog has not checked in since <span class="nw">2026-11-08</span> 22:05 UTC &amp; &lt;now&gt;`
	if got != want {
		t.Errorf("whole = %q", got)
	}
}
