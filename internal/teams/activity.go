package teams

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Account is a person or the bot in a conversation.
type Account struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	AADObjectID string `json:"aadObjectId,omitempty"`
}

// Conversation is where an activity happened.
type Conversation struct {
	ID               string `json:"id"`
	ConversationType string `json:"conversationType,omitempty"`
	TenantID         string `json:"tenantId,omitempty"`
}

// Activity is what the bot service sends the bot, as far as the bot reads it.
type Activity struct {
	Type           string          `json:"type"`
	ID             string          `json:"id,omitempty"`
	Name           string          `json:"name,omitempty"`
	Action         string          `json:"action,omitempty"`
	ServiceURL     string          `json:"serviceUrl"`
	ChannelID      string          `json:"channelId"`
	From           Account         `json:"from"`
	Recipient      Account         `json:"recipient"`
	Conversation   Conversation    `json:"conversation"`
	MembersAdded   []Account       `json:"membersAdded,omitempty"`
	MembersRemoved []Account       `json:"membersRemoved,omitempty"`
	ChannelData    ChannelData     `json:"channelData"`
	Value          json.RawMessage `json:"value,omitempty"`
}

// ChannelData is what Teams adds to an activity: the team, its channel, and
// the tenant.
type ChannelData struct {
	EventType string `json:"eventType,omitempty"`
	Team      *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team,omitempty"`
	Channel *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channel,omitempty"`
	Tenant *struct {
		ID string `json:"id"`
	} `json:"tenant,omitempty"`
}

// Tenant is the tenant the activity came from.
func (a Activity) Tenant() string {
	if a.ChannelData.Tenant != nil && a.ChannelData.Tenant.ID != "" {
		return a.ChannelData.Tenant.ID
	}
	return a.Conversation.TenantID
}

// Includes says whether the bot is among the accounts.
func (b *Bot) Includes(accounts []Account, recipient Account) bool {
	for _, a := range accounts {
		if a.ID == recipient.ID || strings.HasSuffix(a.ID, ":"+b.cfg.AppID) || a.ID == b.cfg.AppID {
			return true
		}
	}
	return false
}

// Card is one Adaptive Card as an attachment.
func Card(card any) map[string]any {
	return map[string]any{"contentType": "application/vnd.microsoft.card.adaptive", "content": card}
}

// PostToChannel starts a new thread in a team's channel with the card.
func (b *Bot) PostToChannel(ctx context.Context, serviceURL, tenantID, channelID string, card any) (int, error) {
	body := map[string]any{
		"isGroup":     true,
		"tenantId":    tenantID,
		"channelData": map[string]any{"channel": map[string]string{"id": channelID}, "tenant": map[string]string{"id": tenantID}},
		"activity":    map[string]any{"type": "message", "attachments": []any{Card(card)}},
	}
	return b.call(ctx, "POST", service(serviceURL)+"/v3/conversations", body, nil)
}

// Reply answers an activity in its own conversation with text.
func (b *Bot) Reply(ctx context.Context, a Activity, text string) error {
	body := map[string]any{"type": "message", "text": text, "replyToId": a.ID, "from": a.Recipient, "recipient": a.From, "conversation": a.Conversation}
	_, err := b.call(ctx, "POST", service(a.ServiceURL)+"/v3/conversations/"+url.PathEscape(a.Conversation.ID)+"/activities/"+url.PathEscape(a.ID), body, nil)
	return err
}

// TeamChannel is one channel of a team the bot is in.
type TeamChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Channels lists a team's channels. Teams names the General channel with no
// name, so it is named here.
func (b *Bot) Channels(ctx context.Context, serviceURL, teamID string) ([]TeamChannel, error) {
	var got struct {
		Conversations []TeamChannel `json:"conversations"`
	}
	if _, err := b.call(ctx, "GET", service(serviceURL)+"/v3/teams/"+url.PathEscape(teamID)+"/conversations", nil, &got); err != nil {
		return nil, err
	}
	for i := range got.Conversations {
		if got.Conversations[i].Name == "" {
			got.Conversations[i].Name = "General"
		}
	}
	return got.Conversations, nil
}

func service(serviceURL string) string { return strings.TrimRight(serviceURL, "/") }

// UserToken is the person's token for the connection the token store holds,
// redeeming the sign-in code when there is one. Empty means the person has
// not signed in.
func (b *Bot) UserToken(ctx context.Context, a Activity, code string) (string, error) {
	q := url.Values{"userId": {a.From.ID}, "connectionName": {b.cfg.Connection}, "channelId": {a.ChannelID}}
	if code != "" {
		q.Set("code", code)
	}
	var got struct {
		Token string `json:"token"`
	}
	status, err := b.call(ctx, "GET", strings.TrimRight(b.cfg.TokenStore, "/")+"/api/usertoken/GetToken?"+q.Encode(), nil, &got)
	if status == 404 {
		return "", nil
	}
	return got.Token, err
}

// Exchange trades the token Teams signed the person in to the bot with for
// their token for the connection. Empty means it could not be traded, and
// the person signs in with the button instead.
func (b *Bot) Exchange(ctx context.Context, a Activity, ssoToken string) (string, error) {
	q := url.Values{"userId": {a.From.ID}, "connectionName": {b.cfg.Connection}, "channelId": {a.ChannelID}}
	var got struct {
		Token string `json:"token"`
	}
	status, err := b.call(ctx, "POST", strings.TrimRight(b.cfg.TokenStore, "/")+"/api/usertoken/exchange?"+q.Encode(), map[string]string{"token": ssoToken}, &got)
	if status >= 400 && status < 500 {
		return "", nil
	}
	return got.Token, err
}

// LoginRequest is the answer to a card action that asks the person to sign
// in: Teams shows a sign-in button on the card, or signs them in silently,
// and sends the action again.
func (b *Bot) LoginRequest(ctx context.Context, a Activity, text string) (map[string]any, error) {
	state, err := json.Marshal(map[string]any{
		"ConnectionName": b.cfg.Connection,
		"MsAppId":        b.cfg.AppID,
		"Conversation": map[string]any{
			"activityId": a.ID, "bot": a.Recipient, "channelId": a.ChannelID,
			"conversation": a.Conversation, "serviceUrl": a.ServiceURL, "user": a.From,
		},
	})
	if err != nil {
		return nil, err
	}
	var res struct {
		SignInLink            string          `json:"signInLink"`
		TokenExchangeResource json.RawMessage `json:"tokenExchangeResource"`
	}
	q := url.Values{"state": {base64.StdEncoding.EncodeToString(state)}}
	if _, err := b.call(ctx, "GET", strings.TrimRight(b.cfg.TokenStore, "/")+"/api/botsignin/GetSignInResource?"+q.Encode(), nil, &res); err != nil {
		return nil, err
	}
	if res.SignInLink == "" {
		return nil, fmt.Errorf("the token store gave no sign-in link")
	}
	value := map[string]any{
		"text":           text,
		"connectionName": b.cfg.Connection,
		"buttons":        []map[string]string{{"type": "signin", "title": "Sign in", "text": "Sign in", "value": res.SignInLink}},
	}
	if len(res.TokenExchangeResource) > 0 && string(res.TokenExchangeResource) != "null" {
		value["tokenExchangeResource"] = res.TokenExchangeResource
	}
	return map[string]any{"statusCode": 401, "type": "application/vnd.microsoft.activity.loginRequest", "value": value}, nil
}

// Member is one person in a team the bot is in.
type Member struct {
	ID                string `json:"id"`
	AadObjectID       string `json:"aadObjectId"`
	Email             string `json:"email"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// Members lists a team's members, page by page.
func (b *Bot) Members(ctx context.Context, serviceURL, teamID string) ([]Member, error) {
	var out []Member
	token := ""
	for range 50 {
		q := url.Values{"pageSize": {"500"}}
		if token != "" {
			q.Set("continuationToken", token)
		}
		var got struct {
			Members           []Member `json:"members"`
			ContinuationToken string   `json:"continuationToken"`
		}
		if _, err := b.call(ctx, "GET", service(serviceURL)+"/v3/conversations/"+url.PathEscape(teamID)+"/pagedmembers?"+q.Encode(), nil, &got); err != nil {
			return nil, err
		}
		out = append(out, got.Members...)
		if token = got.ContinuationToken; token == "" {
			break
		}
	}
	return out, nil
}

// PostToPerson opens the bot's chat with one person, or finds it again, and
// posts the card there.
func (b *Bot) PostToPerson(ctx context.Context, serviceURL, tenantID, memberID string, card any) (int, error) {
	var conv struct {
		ID string `json:"id"`
	}
	body := map[string]any{
		"isGroup":     false,
		"bot":         map[string]string{"id": b.cfg.AppID},
		"members":     []map[string]string{{"id": memberID}},
		"tenantId":    tenantID,
		"channelData": map[string]any{"tenant": map[string]string{"id": tenantID}},
	}
	if status, err := b.call(ctx, "POST", service(serviceURL)+"/v3/conversations", body, &conv); err != nil {
		return status, err
	}
	if conv.ID == "" {
		return 0, fmt.Errorf("the bot service opened no chat")
	}
	return b.call(ctx, "POST", service(serviceURL)+"/v3/conversations/"+url.PathEscape(conv.ID)+"/activities",
		map[string]any{"type": "message", "attachments": []any{Card(card)}}, nil)
}
