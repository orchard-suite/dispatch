// Package azuremonitor reads the webhook an Azure Monitor action group
// posts in the common alert schema, and checks the Entra token a secure
// webhook carries. Every alert type reads; a Prometheus rule group's alert
// keeps its own labels and annotations, so it folds like Alertmanager's.
package azuremonitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/alertmanager"
	"github.com/orchard-suite/dispatch/internal/signed"
)

// SchemaID is the common alert schema's id. An action group sends it when
// the receiver has the common alert schema turned on.
const SchemaID = "azureMonitorCommonAlertSchema"

// MaxBody is the largest webhook body read. One alert is a few kilobytes.
const MaxBody = 1 << 20

// maxLabels bounds the labels an alert carries onto the desk.
const maxLabels = 40

// ErrSchema is a body that is not the common alert schema.
var ErrSchema = errors.New("the body is not an Azure Monitor alert in the common alert schema")

// Payload is one alert, as the action group sends it.
type Payload struct {
	SchemaID string `json:"schemaId"`
	Data     struct {
		Essentials       Essentials        `json:"essentials"`
		AlertContext     json.RawMessage   `json:"alertContext"`
		CustomProperties map[string]string `json:"customProperties"`
	} `json:"data"`
}

// Essentials are the fields every alert type shares.
type Essentials struct {
	AlertID           string   `json:"alertId"`
	AlertRule         string   `json:"alertRule"`
	Severity          string   `json:"severity"`
	SignalType        string   `json:"signalType"`
	MonitorCondition  string   `json:"monitorCondition"`
	MonitoringService string   `json:"monitoringService"`
	AlertTargetIDs    []string `json:"alertTargetIDs"`
	FiredDateTime     string   `json:"firedDateTime"`
	ResolvedDateTime  string   `json:"resolvedDateTime"`
	Description       string   `json:"description"`
	InvestigationLink string   `json:"investigationLink"`
}

// prometheus is the alert context of a Prometheus rule group's alert.
type prometheus struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Expression  string            `json:"expression"`
	Value       string            `json:"expressionValue"`
}

// Parse reads and checks a webhook body.
func Parse(r io.Reader) (Payload, error) {
	var p Payload
	if err := json.NewDecoder(io.LimitReader(r, MaxBody)).Decode(&p); err != nil {
		return p, fmt.Errorf("%w: %v", ErrSchema, err)
	}
	e := p.Data.Essentials
	switch {
	case p.SchemaID != SchemaID:
		return p, fmt.Errorf("%w: schemaId is %q", ErrSchema, p.SchemaID)
	case e.AlertRule == "":
		return p, errors.New("the alert names no alertRule")
	case e.MonitorCondition != "Fired" && e.MonitorCondition != "Resolved":
		return p, fmt.Errorf("monitorCondition must be Fired or Resolved, not %q", e.MonitorCondition)
	}
	return p, nil
}

// Severity is the desk's severity for an Azure severity: Sev0 and Sev1 are
// critical, Sev2 a warning, and the rest info.
func Severity(sev string) string {
	switch sev {
	case "Sev0", "Sev1":
		return "critical"
	case "Sev2":
		return "warning"
	}
	return "info"
}

// Alert is the desk's alert. A Prometheus alert keeps its labels and
// annotations; any other alert is named by its rule, and its service is
// the customProperties' service, else the name of the resource it fired
// on. Custom properties become labels, so an action rule can add
// notify=sms or a severity; they never rename the alert. The fingerprint is the rule and its labels, so a firing and
// its resolve thread together, as Alertmanager's do.
func (p Payload) Alert() alertmanager.Alert {
	e := p.Data.Essentials
	labels := map[string]string{}
	annotations := map[string]string{}
	if strings.EqualFold(e.MonitoringService, "Prometheus") {
		var ctx prometheus
		if json.Unmarshal(p.Data.AlertContext, &ctx) == nil {
			for k, v := range ctx.Labels {
				labels[k] = v
			}
			for k, v := range ctx.Annotations {
				annotations[k] = v
			}
			if ctx.Value != "" {
				annotations["value"] = ctx.Value
			}
		}
	}
	keys := make([]string, 0, len(p.Data.CustomProperties))
	for k := range p.Data.CustomProperties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, set := labels[k]; !set && k != "alertname" && len(labels) < maxLabels && len(p.Data.CustomProperties[k]) <= 200 {
			labels[k] = p.Data.CustomProperties[k]
		}
	}
	setDefault(labels, "alertname", e.AlertRule)
	setDefault(labels, "severity", Severity(e.Severity))
	if target := firstTarget(e.AlertTargetIDs); target != "" {
		setDefault(labels, "azure_resource", target)
		if labels["namespace"] == "" && labels["app"] == "" && labels["job"] == "" {
			setDefault(labels, "service", target)
		}
	}
	setDefault(annotations, "summary", firstOf(e.Description, e.AlertRule))
	a := alertmanager.Alert{
		Status:      "firing",
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    when(e.FiredDateTime),
		Fingerprint: fingerprint(e.AlertRule, labels),
	}
	if strings.HasPrefix(e.InvestigationLink, "https://") {
		a.GeneratorURL = e.InvestigationLink
	}
	if e.MonitorCondition == "Resolved" {
		a.Status = "resolved"
		a.EndsAt = when(e.ResolvedDateTime)
		if a.EndsAt.IsZero() {
			a.EndsAt = time.Now().UTC()
		}
	}
	return a
}

func setDefault(m map[string]string, k, v string) {
	if m[k] == "" && v != "" {
		m[k] = v
	}
}

func firstOf(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// firstTarget is the last segment of the first resource the alert fired
// on, such as a cluster's or a database's name.
func firstTarget(ids []string) string {
	for _, id := range ids {
		if id = strings.TrimRight(id, "/"); id != "" {
			return strings.ToLower(id[strings.LastIndex(id, "/")+1:])
		}
	}
	return ""
}

func when(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func fingerprint(rule string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(rule))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k + "=" + labels[k]))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Azure Monitor's own app, "AzNS AAD Webhook", which asks Entra for a
// secure webhook's token. Every action group in every tenant signs in as
// it, so the token's audience and role are what tie it to this desk.
const (
	ActionGroupsAppID = "461e8683-5575-4561-ac7f-899cc907d62a"
	// Role is the app role the desk's app gives Azure Monitor's app. Only
	// an owner of the desk's app may point an action group at it.
	Role = "ActionGroupsSecureWebhook"
)

// ErrRefused is a token that Azure Monitor did not get for this desk.
var ErrRefused = errors.New("not an Azure Monitor token for this desk")

// Door checks a secure webhook's token: signed by Entra for the tenant,
// issued to Azure Monitor's app, for the desk's app, holding its role.
type Door struct {
	appID  string
	tenant string
	keys   *signed.Keys
}

// DoorConfig is the desk's app and its tenant. LoginURL is Microsoft's
// public cloud when empty; tests point it at their own server.
type DoorConfig struct {
	AppID    string
	TenantID string
	LoginURL string
	Client   *http.Client
	Now      func() time.Time
}

// NewDoor makes the door. It reads nothing until a token comes.
func NewDoor(cfg DoorConfig) (*Door, error) {
	if cfg.AppID == "" || cfg.TenantID == "" {
		return nil, errors.New("the Azure Monitor door needs the desk's app id and its tenant")
	}
	if cfg.LoginURL == "" {
		cfg.LoginURL = "https://login.microsoftonline.com"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 20 * time.Second}
	}
	openID := strings.TrimRight(cfg.LoginURL, "/") + "/" + cfg.TenantID + "/v2.0/.well-known/openid-configuration"
	return &Door{appID: cfg.AppID, tenant: cfg.TenantID, keys: signed.NewKeys(openID, cfg.Client, cfg.Now)}, nil
}

// Check admits an Authorization header whose token Entra signed for this
// tenant, for Azure Monitor's app, to the desk's app, with the role. A
// refusal wraps ErrRefused; a failure to read Entra's keys does not.
func (d *Door) Check(ctx context.Context, authorization string) error {
	claims, err := d.keys.Check(ctx, authorization)
	switch {
	case errors.Is(err, signed.ErrRefused):
		return fmt.Errorf("%w: %w", ErrRefused, err)
	case err != nil:
		return fmt.Errorf("Entra's keys: %w", err)
	case claims.Text("tid") != d.tenant:
		return fmt.Errorf("%w: another tenant", ErrRefused)
	case claims.Text("iss") != "https://sts.windows.net/"+d.tenant+"/" && claims.Text("iss") != "https://login.microsoftonline.com/"+d.tenant+"/v2.0":
		return fmt.Errorf("%w: issued by %q", ErrRefused, claims.Text("iss"))
	case !claims.For(d.appID) && !claims.For("api://"+d.appID):
		return fmt.Errorf("%w: for another app", ErrRefused)
	case firstOf(claims.Text("appid"), claims.Text("azp")) != ActionGroupsAppID:
		return fmt.Errorf("%w: not Azure Monitor's app", ErrRefused)
	case !claims.Has("roles", Role):
		return fmt.Errorf("%w: without the %s role", ErrRefused, Role)
	}
	return nil
}

// UseClient reads Entra's keys through c, for a test against its own TLS server.
func (d *Door) UseClient(c *http.Client) { d.keys.UseClient(c) }

// LooksSigned says whether an Authorization header carries a JWT, which an
// issued token or the shared secret never is.
func LooksSigned(authorization string) bool {
	raw, ok := strings.CutPrefix(authorization, "Bearer ")
	return ok && strings.Count(strings.TrimSpace(raw), ".") == 2
}
