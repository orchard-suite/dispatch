package outbox

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/seal"
	"github.com/orchard-suite/dispatch/internal/store"
)

type hook struct {
	mu     sync.Mutex
	bodies []string
	status int
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bodies = append(h.bodies, string(b))
	w.WriteHeader(h.status)
}

func setup(t *testing.T, h *hook) (*Outbox, *store.Store, *time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sealer, _ := seal.New("test key")
	reach := func(_ context.Context, ref notice.Ref) []string {
		if ref.ID() == "azure_app_service:beacon-api-prod" {
			return []string{"program:4242"}
		}
		return nil
	}
	o := New(st, sealer, reach, slog.New(slog.DiscardHandler), func(context.Context, string) {})
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	o.Now = func() time.Time { return now }
	o.client = &http.Client{Transport: http.DefaultTransport}
	return o, st, &now
}

func approval(key, app string) notice.Event {
	return notice.Event{Producer: "orrery", Kind: "action.awaiting_approval", Key: key, Title: "Restart " + app + " waits for approval",
		Entity: &notice.Ref{Blueprint: "azure_app_service", Key: app}}
}

func TestRoutesByAnchorOnceAndRetries(t *testing.T) {
	h := &hook{status: http.StatusServiceUnavailable}
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	o, st, now := setup(t, h)
	o.client = srv.Client()
	ctx := context.Background()
	ch, _, err := o.AddChannel(ctx, "Beacon devops", notice.Teams, srv.URL+"/hook?sig=secret", "admin@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ch.Target, "secret") {
		t.Fatal("the channel's URL was stored in the clear")
	}
	// Two subscriptions both take it; the channel still gets it once.
	for _, topics := range [][]string{{"action.*"}, {"*"}} {
		if _, err := st.AddSubscription(ctx, notice.Subscription{ChannelID: ch.ID, Topics: topics, Anchor: "program:4242", CreatedAt: *now}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := o.Accept(ctx, approval("run-1", "beacon-api-prod"))
	if err != nil || got.Queued != 1 {
		t.Fatalf("queued %d, %v", got.Queued, err)
	}
	if other, _ := o.Accept(ctx, approval("run-2", "globex-api-prod")); other.Queued != 0 {
		t.Fatalf("another program's app was queued")
	}
	if again, _ := o.Accept(ctx, approval("run-1", "beacon-api-prod")); !again.Repeat || again.Queued != 0 {
		t.Fatalf("a repeat queued again: %+v", again)
	}

	o.SendDue(ctx)
	lines, _ := st.RecentDeliveries(ctx, 10)
	if len(lines) != 1 || lines[0].State != notice.Pending || lines[0].Attempts != 1 || lines[0].Status != 503 {
		t.Fatalf("after a refused try: %+v", lines)
	}
	// Not due yet: nothing is sent.
	o.SendDue(ctx)
	if len(h.bodies) != 1 {
		t.Fatalf("sent before its wait: %d", len(h.bodies))
	}
	h.status = http.StatusAccepted
	*now = now.Add(time.Minute)
	o.SendDue(ctx)
	lines, _ = st.RecentDeliveries(ctx, 10)
	if lines[0].State != notice.Sent || lines[0].Status != 202 || lines[0].SentAt == nil {
		t.Fatalf("not marked sent: %+v", lines[0])
	}
	var msg struct{ Type string }
	if err := json.Unmarshal([]byte(h.bodies[1]), &msg); err != nil || msg.Type != "message" {
		t.Fatalf("not a Teams message: %s", h.bodies[1])
	}
}

func TestGivesUpOnAPermanentRefusal(t *testing.T) {
	h := &hook{status: http.StatusNotFound}
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	o, st, now := setup(t, h)
	o.client = srv.Client()
	ctx := context.Background()
	ch, _, _ := o.AddChannel(ctx, "gone", notice.Webhook, srv.URL, "admin")
	st.AddSubscription(ctx, notice.Subscription{ChannelID: ch.ID, Topics: []string{"*"}, CreatedAt: *now})
	o.Accept(ctx, approval("run-1", "beacon-api-prod"))
	o.SendDue(ctx)
	lines, _ := st.RecentDeliveries(ctx, 10)
	if lines[0].State != notice.Failed || !strings.Contains(lines[0].LastError, "404") {
		t.Fatalf("a 404 was tried again: %+v", lines[0])
	}
	if strings.Contains(lines[0].LastError, srv.URL) {
		t.Error("the error names the secret URL")
	}
}

func TestChannelNeedsHTTPSAndAKey(t *testing.T) {
	o, _, _ := setup(t, &hook{})
	if _, _, err := o.AddChannel(context.Background(), "x", notice.Teams, "http://example.org/hook", "a"); err == nil {
		t.Error("took a plain http URL")
	}
	o.seal, _ = seal.New("")
	if _, _, err := o.AddChannel(context.Background(), "x", notice.Teams, "https://example.org/hook", "a"); err == nil {
		t.Error("kept a channel without SECRET_KEY")
	}
}
