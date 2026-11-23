package server

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchard-suite/dispatch/internal/discord"
	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestADiscordPressActsAsItsPerson(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(nil)
	bot, err := discord.New("bot-token", hex.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	h.srv.UseDiscord(bot)
	st := h.desk.Store
	st.SavePerson(ctx, notice.Person{Email: member, DiscordID: "123456789012345678"})
	st.SaveChain(ctx, notice.Chain{Slug: "oncall", Name: "On call", Steps: []notice.Step{{Targets: []notice.Target{{Person: member}}, Wait: 5}}})
	if _, err := h.desk.Notices().Accept(ctx, notice.Event{Producer: "grafana", Kind: "alert.disk", Key: "d1", Title: "Disk full", To: []notice.Target{{Chain: "oncall"}}}); err != nil {
		t.Fatal(err)
	}
	press := func(body string, sign bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/discord", strings.NewReader(body))
		sig := ed25519.Sign(priv, append([]byte("1700000000"), body...))
		if !sign {
			sig[0] ^= 1
		}
		req.Header.Set(discord.SignatureHeader, hex.EncodeToString(sig))
		req.Header.Set(discord.TimestampHeader, "1700000000")
		return h.do(h.api, req)
	}
	if rec := press(`{"type":1}`, true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"type":1`) {
		t.Fatalf("ping %d %s", rec.Code, rec.Body.String())
	}
	if rec := press(`{"type":3,"data":{"custom_id":"ack:1"},"user":{"id":"123456789012345678"}}`, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unsigned press: %d", rec.Code)
	}
	if rec := press(`{"type":3,"data":{"custom_id":"ack:1"},"user":{"id":"555555555555555555"}}`, true); !strings.Contains(rec.Body.String(), "nobody's Me page") {
		t.Errorf("an account nobody has: %s", rec.Body.String())
	}
	if p, _ := st.Page(ctx, 1); p.State != notice.PageActive {
		t.Fatal("a stranger acknowledged the page")
	}
	rec := press(`{"type":3,"data":{"custom_id":"ack:1"},"member":{"user":{"id":"123456789012345678"}}}`, true)
	if !strings.Contains(rec.Body.String(), "You have it") || !strings.Contains(rec.Body.String(), `"flags":64`) {
		t.Errorf("press %s", rec.Body.String())
	}
	if p, _ := st.Page(ctx, 1); p.State != notice.PageAcknowledged || p.AcknowledgedBy != member {
		t.Errorf("page %+v", p)
	}
}
