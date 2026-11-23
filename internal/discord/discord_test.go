package discord

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestVerifyHoldsOnlyForDiscordsSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	b, err := New("bot-token", hex.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"type":1}`)
	sig := hex.EncodeToString(ed25519.Sign(priv, append([]byte("1700000000"), body...)))
	if !b.Verify(sig, "1700000000", body) {
		t.Error("a good signature")
	}
	if b.Verify(sig, "1700000001", body) || b.Verify(sig, "1700000000", []byte(`{"type":3}`)) || b.Verify("zz", "1", body) {
		t.Error("a signature over anything else")
	}
	if _, err := New("t", "nothex"); err == nil {
		t.Error("a bad key")
	}
}

func TestSendPersonOpensTheirMessagesAndPostsButtons(t *testing.T) {
	var posted map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot bot-token" {
			w.WriteHeader(401)
			return
		}
		paths = append(paths, r.Method+" "+r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/users/@me/channels":
			var in map[string]string
			json.Unmarshal(raw, &in)
			if in["recipient_id"] == "999999999999999999" {
				w.WriteHeader(403)
				w.Write([]byte(`{"message":"Cannot send messages to this user","code":50007}`))
				return
			}
			w.Write([]byte(`{"id":"dm-1"}`))
		case "/channels/dm-1/messages":
			json.Unmarshal(raw, &posted)
			w.Write([]byte(`{"id":"m"}`))
		}
	}))
	defer srv.Close()
	pub, _, _ := ed25519.GenerateKey(nil)
	b, _ := New("bot-token", hex.EncodeToString(pub))
	b.UseAPI(srv.URL, srv.Client())
	out := notice.Outgoing{Person: notice.Person{DiscordID: "123456789012345678"}, Event: notice.Event{Producer: "grafana", Kind: "alert.disk", Title: "Disk full", Severity: "critical"},
		Ack: "https://dispatch.test/pages/7", PageID: 7}
	if status, err := b.SendPerson(context.Background(), out); err != nil || status != 200 {
		t.Fatalf("send %d %v", status, err)
	}
	if strings.Join(paths, ",") != "POST /users/@me/channels,POST /channels/dm-1/messages" {
		t.Errorf("calls %v", paths)
	}
	raw, _ := json.Marshal(posted)
	for _, want := range []string{`"custom_id":"ack:7"`, `"custom_id":"esc:7"`, `"url":"https://dispatch.test/pages/7"`, `"title":"Disk full"`, `"parse":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("posted %s, want %s", raw, want)
		}
	}
	out.Person.DiscordID = "999999999999999999"
	if _, err := b.SendPerson(context.Background(), out); !errors.Is(err, notice.ErrUnreachable) {
		t.Errorf("a person who shares no server with the bot: %v", err)
	}
	hook, _ := Webhook(notice.Event{Title: "x", Links: []notice.Link{{Title: "a", URL: "https://a.example"}}})
	if strings.Contains(string(hook), "components") {
		t.Errorf("a webhook post carries no buttons: %s", hook)
	}
}
