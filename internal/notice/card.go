package notice

import (
	"encoding/json"
	"fmt"
)

// Body is what a channel of the kind is sent for the event. A bot channel
// is sent its card by the bot (BotCard), not a body.
func Body(kind Kind, e Event) ([]byte, error) {
	switch kind {
	case Teams:
		return json.Marshal(teamsMessage(e))
	case Webhook:
		e.Page = ""
		return json.Marshal(map[string]any{"event": e})
	}
	return nil, fmt.Errorf("%w: no channel kind %q", ErrInvalid, kind)
}

// teamsMessage is the envelope a Teams Workflows webhook takes: a message
// with one Adaptive Card. A webhook card cannot act, so it shows the links
// and leaves the actions out.
func teamsMessage(e Event) map[string]any {
	return map[string]any{
		"type": "message",
		"attachments": []map[string]any{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"contentUrl":  nil,
			"content":     card(e, nil, ""),
		}},
	}
}

// ActVerb is the Action.Execute verb a bot card's buttons send; their data
// names the event and the action's place in it.
const ActVerb = "act"

// ActData is the data a bot card's button sends back.
type ActData struct {
	Event  int64 `json:"event"`
	Action int   `json:"action"`
}

// BotCard is the card the bot posts: the event, its actions as buttons that
// call back to the bot, and its links. After someone acted, note says what
// they did and the buttons are gone.
func BotCard(e Event, note string) map[string]any {
	if note != "" {
		return card(e, nil, note)
	}
	buttons := make([]map[string]any, 0, len(e.Actions))
	for i, a := range e.Actions {
		b := map[string]any{"type": "Action.Execute", "title": a.Title, "verb": ActVerb, "data": ActData{Event: e.ID, Action: i}}
		if a.Style != "" {
			b["style"] = a.Style
		}
		buttons = append(buttons, b)
	}
	return card(e, buttons, "")
}

// card is the event as an Adaptive Card. Teams renders version 1.5; 1.4 is
// used so older clients render it too, and it is the first with
// Action.Execute.
func card(e Event, buttons []map[string]any, note string) map[string]any {
	body := []map[string]any{
		{"type": "TextBlock", "text": e.Title, "weight": "Bolder", "size": "Medium", "wrap": true, "color": teamsColor(e.Severity)},
	}
	if e.Text != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": e.Text, "wrap": true})
	}
	facts := make([]map[string]string, 0, len(e.Facts)+2)
	if e.Entity != nil {
		name := e.Entity.Title
		if name == "" {
			name = e.Entity.Key
		}
		facts = append(facts, map[string]string{"title": "About", "value": name})
	}
	for _, f := range e.Facts {
		facts = append(facts, map[string]string{"title": f.Name, "value": f.Value})
	}
	if e.Severity != "" {
		facts = append(facts, map[string]string{"title": "Severity", "value": e.Severity})
	}
	if len(facts) > 0 {
		body = append(body, map[string]any{"type": "FactSet", "facts": facts})
	}
	if note != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": note, "weight": "Bolder", "wrap": true})
	}
	body = append(body, map[string]any{"type": "TextBlock", "text": "From " + e.Producer + " · " + e.At.UTC().Format("2006-01-02 15:04 UTC"), "isSubtle": true, "size": "Small", "wrap": true})
	c := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"msteams": map[string]any{"width": "Full"},
		"body":    body,
	}
	actions := buttons
	for _, l := range e.Links {
		actions = append(actions, map[string]any{"type": "Action.OpenUrl", "title": l.Title, "url": l.URL})
	}
	if len(actions) > 0 {
		c["actions"] = actions
	}
	return c
}

func teamsColor(severity string) string {
	switch severity {
	case "critical":
		return "Attention"
	case "warning":
		return "Warning"
	}
	return "Default"
}
