package teams

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/orchard-suite/dispatch/internal/signed"
)

// ErrRefused is a request the bot service did not sign for this bot.
var ErrRefused = errors.New("not from the bot service")

// Verify checks the token on a request from the Bot Connector: signed by a
// key in the bot service's published set, issued by the bot service, for
// this bot, in its time, and naming the service URL the activity came from.
// Every check is required; none can be turned off.
func (b *Bot) Verify(ctx context.Context, authorization, serviceURL string) error {
	claims, err := b.keys.Check(ctx, authorization)
	switch {
	case errors.Is(err, signed.ErrRefused):
		return fmt.Errorf("%w: %w", ErrRefused, err)
	case err != nil:
		return fmt.Errorf("the bot service's keys: %w", err)
	case claims["iss"] != botIssuer:
		return fmt.Errorf("%w: issued by %v", ErrRefused, claims["iss"])
	case !claims.For(b.cfg.AppID):
		return fmt.Errorf("%w: for another bot", ErrRefused)
	}
	named := ""
	for k, v := range claims {
		if strings.EqualFold(k, "serviceurl") {
			named, _ = v.(string)
		}
	}
	if named == "" || !sameService(named, serviceURL) {
		return fmt.Errorf("%w: the token names another service URL", ErrRefused)
	}
	return nil
}

func sameService(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}
