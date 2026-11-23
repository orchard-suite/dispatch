package sms

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestSendSignsInAndAsksForAReport(t *testing.T) {
	var mu sync.Mutex
	var sent map[string]any
	refuse := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/tenant-1/oauth2/v2.0/token":
			_ = r.ParseForm()
			if r.Form.Get("client_assertion") != "workload-token" || r.Form.Get("scope") != "https://communication.azure.com/.default" || r.Form.Get("client_secret") != "" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"access_token":"acs-token","expires_in":3600}`))
		case "/sms":
			if r.Header.Get("Authorization") != "Bearer acs-token" || r.URL.Query().Get("api-version") != apiVersion {
				w.WriteHeader(401)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &sent)
			w.WriteHeader(202)
			if refuse {
				w.Write([]byte(`{"value":[{"to":"+15555550100","httpStatusCode":400,"successful":false,"errorMessage":"Invalid To phone number format."}]}`))
				return
			}
			w.Write([]byte(`{"value":[{"to":"+15555550100","messageId":"m-1","httpStatusCode":202,"successful":true}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("workload-token"), 0o600)
	c, err := New(Config{Endpoint: srv.URL, From: "+15555550199", AppID: "sms-app", TenantID: "tenant-1", TokenFile: file, LoginURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.UseClient(srv.Client())
	out := notice.Outgoing{Person: notice.Person{Phone: "+15555550100"}, Event: notice.Event{Title: "Disk full"}, Ack: "https://dispatch.test/pages/4", MessageID: 12}
	if status, err := (Person{C: c}).SendPerson(context.Background(), out); err != nil || status != 202 {
		t.Fatalf("send %d %v", status, err)
	}
	opts := sent["smsSendOptions"].(map[string]any)
	if sent["from"] != "+15555550199" || opts["enableDeliveryReport"] != true || opts["tag"] != Tag(12) || MessageOf(opts["tag"].(string)) != 12 {
		t.Errorf("sent %v", sent)
	}
	if text := sent["message"].(string); !strings.HasPrefix(text, "PAGE: Disk full") || !strings.Contains(text, "Reply ACK") || !strings.HasSuffix(text, "/pages/4") {
		t.Errorf("text %q", text)
	}
	refuse = true
	if status, err := c.Send(context.Background(), "+1", "x", ""); err == nil || status != 400 {
		t.Errorf("a refused number is the number's status: %d %v", status, err)
	}
	if _, err := (Person{C: c}).SendPerson(context.Background(), notice.Outgoing{}); !errors.Is(err, notice.ErrUnreachable) {
		t.Errorf("no number: %v", err)
	}
}

func TestRepliesAndGridEvents(t *testing.T) {
	for text, want := range map[string]Reply{"ack": ReplyAcknowledge, " Yes. ": ReplyAcknowledge, "ESC please": ReplyEscalate, "who is this": ReplyUnknown} {
		if got := ReadReply(text); got != want {
			t.Errorf("%q: %q", text, got)
		}
	}
	events, err := ParseGrid(strings.NewReader(`[{"id":"1","eventType":"Microsoft.EventGrid.SubscriptionValidationEvent","data":{"validationCode":"abc"}}]`))
	if err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	if code, err := ValidationCode(events[0]); code != "abc" || err != nil {
		t.Errorf("code %q %v", code, err)
	}
	if MessageOf("someone-else") != 0 {
		t.Error("a tag not ours names no message")
	}
}
