package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/orchard-suite/dispatch/internal/discord"
	"github.com/orchard-suite/dispatch/internal/store"
)

// UseDiscord answers the Discord bot's buttons.
func (s *Server) UseDiscord(b *discord.Bot) { s.discord = b }

// apiDiscord is the Discord bot's door. It trusts only a body Discord
// signed with the application's key, and acts only as the person whose
// Discord account the press came from, as that person.
func (s *Server) apiDiscord(w http.ResponseWriter, r *http.Request) {
	if s.discord == nil {
		writeError(w, http.StatusNotFound, "this desk has no Discord bot")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, discord.MaxInteraction))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body could not be read")
		return
	}
	if !s.discord.Verify(r.Header.Get(discord.SignatureHeader), r.Header.Get(discord.TimestampHeader), body) {
		s.logRequest(r.Context(), slog.LevelWarn, "discord refused: bad signature", r)
		writeError(w, http.StatusUnauthorized, "bad signature")
		return
	}
	var in discord.Interaction
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, "not an interaction")
		return
	}
	s.logRequest(r.Context(), slog.LevelDebug, "discord admitted", r, "type", in.Type)
	switch in.Type {
	case discord.Ping:
		writeJSON(w, http.StatusOK, map[string]int{"type": discord.Pong})
		return
	case discord.Component:
		writeJSON(w, http.StatusOK, discord.Reply(s.discordPress(r, in)))
		return
	}
	writeJSON(w, http.StatusOK, discord.Reply("dispatch does not know that."))
}

// discordPress acknowledges a page, or passes it on, as the person whose
// account pressed, and says what it did.
func (s *Server) discordPress(r *http.Request, in discord.Interaction) string {
	ctx := r.Context()
	verb, idText, _ := strings.Cut(in.Data.CustomID, ":")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || (verb != "ack" && verb != "esc") {
		return "dispatch does not know that button."
	}
	p, err := s.store.PersonByDiscord(ctx, in.Who())
	if errors.Is(err, store.ErrNotFound) {
		s.logRequest(ctx, slog.LevelWarn, "discord press refused: no person has this account", r)
		s.metrics.Reply(ctx, "discord", "refused")
		return "This Discord account is on nobody's Me page in dispatch, so it cannot act."
	}
	if err != nil {
		s.logRequest(ctx, slog.LevelError, "discord person not read", r, "error", err)
		return "dispatch could not read who you are; try the page's link."
	}
	box := s.desk.Notices()
	if box == nil {
		return "This desk sends no notices."
	}
	var moved bool
	if verb == "ack" {
		moved, err = box.Acknowledge(ctx, id, p.Email)
	} else {
		moved, err = box.Escalate(ctx, id, p.Email)
	}
	if err != nil {
		s.logRequest(ctx, slog.LevelError, "discord press failed", r, "page", id, "verb", verb, "error", err)
		return "That did not work; try the page's link."
	}
	s.logRequest(ctx, slog.LevelInfo, "discord press", r, "page", id, "verb", verb, "moved", moved)
	s.metrics.Reply(ctx, "discord", map[string]string{"ack": "acknowledged", "esc": "escalated"}[verb])
	switch {
	case !moved:
		return "This page had already ended."
	case verb == "ack":
		return "You have it. Nobody else in the chain is told."
	}
	return "Passed on: the next step is told now."
}
