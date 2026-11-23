package teams_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/teams"
	"github.com/orchard-suite/dispatch/internal/teams/teamstest"
)

func TestVerifyTakesOnlyTheBotServicesTokenForThisBot(t *testing.T) {
	svc := teamstest.New(t)
	bot := svc.Bot(t)
	ctx := context.Background()
	good := svc.Claims()
	if err := bot.Verify(ctx, "Bearer "+svc.Sign(good), svc.URL); err != nil {
		t.Fatalf("a good token was refused: %v", err)
	}
	bad := map[string]func(map[string]any){
		"another bot":         func(c map[string]any) { c["aud"] = "other-bot" },
		"another issuer":      func(c map[string]any) { c["iss"] = "https://evil.example.com" },
		"expired":             func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"not yet":             func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() },
		"another service URL": func(c map[string]any) { c["serviceurl"] = "https://evil.example.com" },
		"no service URL":      func(c map[string]any) { delete(c, "serviceurl") },
	}
	for name, change := range bad {
		claims := svc.Claims()
		change(claims)
		if err := bot.Verify(ctx, "Bearer "+svc.Sign(claims), svc.URL); !errors.Is(err, teams.ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	forged := teamstest.New(t)
	if err := bot.Verify(ctx, "Bearer "+forged.Sign(svc.Claims()), svc.URL); !errors.Is(err, teams.ErrRefused) {
		t.Errorf("a token signed by another key: %v", err)
	}
	for _, header := range []string{"", "Bearer ", "Basic abc", "Bearer a.b"} {
		if err := bot.Verify(ctx, header, svc.URL); !errors.Is(err, teams.ErrRefused) {
			t.Errorf("%q: %v", header, err)
		}
	}
}
