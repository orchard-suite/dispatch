// Package sms sends one text through Azure Communication Services, signed
// in with the pod's federated workload token, and reads what Event Grid
// delivers back: texts people send to the number, and delivery reports.
package sms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/entra"
	"github.com/orchard-suite/dispatch/internal/notice"
)

// Config is the Communication Services resource, its number, and the Entra
// app that signs in to it.
type Config struct {
	// Endpoint is the resource, such as https://name.communication.azure.com.
	Endpoint string
	// From is the resource's number, in +country form.
	From      string
	AppID     string
	TenantID  string
	TokenFile string
	LoginURL  string
}

const apiVersion = "2021-03-07"

// MaxText is how long a text may be: two segments of 160 GSM-7 characters,
// since each segment is billed.
const MaxText = 320

// Client sends texts from one number.
type Client struct {
	cfg    Config
	sign   *entra.Signer
	client *http.Client
}

func New(cfg Config) (*Client, error) {
	if !strings.HasPrefix(cfg.Endpoint, "https://") || cfg.From == "" {
		return nil, errors.New("sms: SMS_ENDPOINT must be the resource's https URL and SMS_FROM its number")
	}
	sign, err := entra.New(entra.Config{Name: "the SMS app", AppID: cfg.AppID, TenantID: cfg.TenantID, TokenFile: cfg.TokenFile,
		Scope: "https://communication.azure.com/.default", LoginURL: cfg.LoginURL})
	if err != nil {
		return nil, err
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	return &Client{cfg: cfg, sign: sign, client: telemetry.Client(20 * time.Second)}, nil
}

// UseClient sends with c, for a test against its own TLS server.
func (c *Client) UseClient(h *http.Client) {
	c.client = h
	c.sign.UseClient(h)
}

// From is the number texts come from.
func (c *Client) From() string { return c.cfg.From }

// Send texts one number, asking for a delivery report tagged with tag, and
// answers the service's status for that number. A sign-in failure is a 0,
// so it is tried again.
func (c *Client) Send(ctx context.Context, to, text, tag string) (int, error) {
	token, err := c.sign.Token(ctx)
	if err != nil {
		return 0, err
	}
	if len(text) > MaxText {
		text = text[:MaxText-1] + "…"
	}
	body, _ := json.Marshal(map[string]any{
		"from":           c.cfg.From,
		"smsRecipients":  []map[string]string{{"to": to}},
		"message":        text,
		"smsSendOptions": map[string]any{"enableDeliveryReport": true, "tag": tag},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint+"/sms?api-version="+apiVersion, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return 0, entra.Unwrap(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode == http.StatusUnauthorized {
		c.sign.Forget()
	}
	if res.StatusCode != http.StatusAccepted {
		var said struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &said)
		return res.StatusCode, fmt.Errorf("the SMS service answered %d %s: %s", res.StatusCode, said.Error.Code, entra.FirstLine(said.Error.Message))
	}
	var got struct {
		Value []struct {
			HTTPStatusCode int    `json:"httpStatusCode"`
			Successful     bool   `json:"successful"`
			ErrorMessage   string `json:"errorMessage"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || len(got.Value) != 1 {
		return res.StatusCode, errors.New("the SMS service's answer could not be read")
	}
	if v := got.Value[0]; !v.Successful {
		return v.HTTPStatusCode, fmt.Errorf("the SMS service refused the number (%d): %s", v.HTTPStatusCode, entra.FirstLine(v.ErrorMessage))
	}
	return res.StatusCode, nil
}

// Event Grid's event types this package reads.
const (
	Validation = "Microsoft.EventGrid.SubscriptionValidationEvent"
	Received   = "Microsoft.Communication.SMSReceived"
	Delivery   = "Microsoft.Communication.SMSDeliveryReportReceived"
)

// GridEvent is one event in Event Grid's schema.
type GridEvent struct {
	ID        string          `json:"id"`
	EventType string          `json:"eventType"`
	Data      json.RawMessage `json:"data"`
}

// Inbound is a text someone sent to the number.
type Inbound struct {
	MessageID string `json:"messageId"`
	From      string `json:"from"`
	To        string `json:"to"`
	Message   string `json:"message"`
}

// Report is the carrier's word on one text dispatch sent.
type Report struct {
	MessageID     string `json:"messageId"`
	To            string `json:"to"`
	Status        string `json:"deliveryStatus"`
	StatusDetails string `json:"deliveryStatusDetails"`
	Tag           string `json:"tag"`
}

// Delivered says whether the report means the phone has it.
func (r Report) Delivered() bool { return strings.EqualFold(r.Status, "Delivered") }

// MaxBody is the most of an Event Grid post this package reads.
const MaxBody = 1 << 20

// ParseGrid reads an Event Grid post, which is a list of events.
func ParseGrid(r io.Reader) ([]GridEvent, error) {
	var events []GridEvent
	if err := json.NewDecoder(io.LimitReader(r, MaxBody)).Decode(&events); err != nil {
		return nil, fmt.Errorf("the body is not a list of Event Grid events: %w", err)
	}
	return events, nil
}

// ValidationCode is the code a subscription's validation event carries.
func ValidationCode(e GridEvent) (string, error) {
	var d struct {
		Code string `json:"validationCode"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil || d.Code == "" {
		return "", errors.New("a validation event without its code")
	}
	return d.Code, nil
}

// Reply is what a person's text asks for.
type Reply string

const (
	ReplyAcknowledge Reply = "acknowledge"
	ReplyEscalate    Reply = "escalate"
	ReplyUnknown     Reply = ""
)

// ReadReply reads a text: ACK (or A, OK, YES) acknowledges the page that
// last texted the person; ESC (or E, NEXT) tells its next step now.
func ReadReply(text string) Reply {
	word := strings.ToUpper(strings.Trim(strings.TrimSpace(text), ".!"))
	if f := strings.Fields(word); len(f) > 0 {
		word = f[0]
	}
	switch word {
	case "ACK", "A", "OK", "YES", "Y":
		return ReplyAcknowledge
	case "ESC", "E", "NEXT", "ESCALATE":
		return ReplyEscalate
	}
	return ReplyUnknown
}

// Person is texting as a medium: the person's own number, the event as a
// short text, tagged with the message's id so its delivery report finds it.
type Person struct{ C *Client }

func (p Person) SendPerson(ctx context.Context, out notice.Outgoing) (int, error) {
	if out.Person.Phone == "" {
		return 0, fmt.Errorf("%w: no SMS number", notice.ErrUnreachable)
	}
	return p.C.Send(ctx, out.Person.Phone, notice.Text(out.Event, out.Ack), Tag(out.MessageID))
}

// Tag is the delivery report tag of a message.
func Tag(messageID int64) string { return fmt.Sprintf("dispatch-message-%d", messageID) }

// MessageOf is the message a delivery report's tag names, or 0.
func MessageOf(tag string) int64 {
	var id int64
	if _, err := fmt.Sscanf(tag, "dispatch-message-%d", &id); err != nil {
		return 0
	}
	return id
}
