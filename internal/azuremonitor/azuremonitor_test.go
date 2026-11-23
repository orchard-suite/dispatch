package azuremonitor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/azuremonitor"
	"github.com/orchard-suite/dispatch/internal/azuremonitor/azuremonitortest"
)

// A Prometheus rule group's alert, as an action group posts it.
const prometheusAlert = `{"schemaId":"azureMonitorCommonAlertSchema","data":{"essentials":{
 "alertId":"/subscriptions/s/providers/Microsoft.AlertsManagement/alerts/a1","alertRule":"PersistentVolumeAlmostFull",
 "severity":"Sev1","signalType":"Metric","monitorCondition":"%s","monitoringService":"Prometheus",
 "alertTargetIDs":["/subscriptions/s/resourcegroups/rg/providers/microsoft.monitor/accounts/ws"],
 "firedDateTime":"2026-11-16T20:00:00.000Z","resolvedDateTime":"2026-11-16T21:00:00.000Z",
 "investigationLink":"https://portal.azure.com/#view/alert/a1"},
 "alertContext":{"interval":"PT1M","expression":"x > 0.9","expressionValue":"0.93","for":"PT5M",
  "labels":{"alertname":"PersistentVolumeAlmostFull","namespace":"beacon-prod","persistentvolumeclaim":"beacon-files","severity":"critical","notify":"sms"},
  "annotations":{"summary":"beacon-files in beacon-prod is 93% full"}},
 "customProperties":null}}`

func parse(t *testing.T, body string) azuremonitor.Payload {
	t.Helper()
	p, err := azuremonitor.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAPrometheusAlertKeepsItsLabelsAndThreadsItsResolve(t *testing.T) {
	fired := parse(t, strings.Replace(prometheusAlert, "%s", "Fired", 1)).Alert()
	if fired.Name() != "PersistentVolumeAlmostFull" || fired.Labels["namespace"] != "beacon-prod" || fired.Labels["notify"] != "sms" || fired.Labels["severity"] != "critical" {
		t.Fatalf("labels %v", fired.Labels)
	}
	if fired.Labels["service"] != "" {
		t.Errorf("a namespace names the service, not the resource: %v", fired.Labels)
	}
	if fired.Annotations["summary"] != "beacon-files in beacon-prod is 93% full" || fired.Annotations["value"] != "0.93" {
		t.Errorf("annotations %v", fired.Annotations)
	}
	if !fired.Firing() || fired.StartsAt != time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC) || fired.GeneratorURL == "" {
		t.Errorf("firing %+v", fired)
	}
	resolved := parse(t, strings.Replace(prometheusAlert, "%s", "Resolved", 1)).Alert()
	if !resolved.Resolved() || resolved.EndsAt != time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC) {
		t.Errorf("resolved %+v", resolved)
	}
	if resolved.Fingerprint != fired.Fingerprint || fired.Fingerprint == "" {
		t.Errorf("a firing and its resolve share a fingerprint: %q %q", fired.Fingerprint, resolved.Fingerprint)
	}
}

func TestAnyOtherAlertIsNamedByItsRuleAndResource(t *testing.T) {
	a := parse(t, `{"schemaId":"azureMonitorCommonAlertSchema","data":{"essentials":{"alertRule":"db-storage-full","severity":"Sev2",
		"monitorCondition":"Fired","monitoringService":"Platform","description":"Storage above 80%",
		"alertTargetIDs":["/subscriptions/s/resourceGroups/rg/providers/Microsoft.Sql/servers/DB-A"]},
		"alertContext":{},"customProperties":{"notify":"sms","alertname":"ignored"}}}`).Alert()
	want := map[string]string{"alertname": "db-storage-full", "severity": "warning", "service": "db-a", "azure_resource": "db-a", "notify": "sms"}
	for k, v := range want {
		if a.Labels[k] != v {
			t.Errorf("%s = %q, want %q (%v)", k, a.Labels[k], v, a.Labels)
		}
	}
	if a.Annotations["summary"] != "Storage above 80%" {
		t.Errorf("summary %v", a.Annotations)
	}
	for sev, want := range map[string]string{"Sev0": "critical", "Sev1": "critical", "Sev2": "warning", "Sev3": "info", "Sev4": "info"} {
		if got := azuremonitor.Severity(sev); got != want {
			t.Errorf("%s is %s, want %s", sev, got, want)
		}
	}
}

func TestParseRefusesWhatIsNotTheCommonSchema(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":       `nope`,
		"another schema": `{"schemaId":"Microsoft.Insights/activityLogs","data":{"essentials":{"alertRule":"r","monitorCondition":"Fired"}}}`,
		"no rule":        `{"schemaId":"azureMonitorCommonAlertSchema","data":{"essentials":{"monitorCondition":"Fired"}}}`,
		"no condition":   `{"schemaId":"azureMonitorCommonAlertSchema","data":{"essentials":{"alertRule":"r"}}}`,
	} {
		if _, err := azuremonitor.Parse(strings.NewReader(body)); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
}

func TestTheDoorTakesOnlyAzureMonitorsTokenForThisDesk(t *testing.T) {
	entra := azuremonitortest.New(t)
	door := entra.Door(t)
	ctx := context.Background()
	if err := door.Check(ctx, entra.Sign(entra.Claims())); err != nil {
		t.Fatalf("a good v1 token was refused: %v", err)
	}
	v2 := entra.Claims()
	delete(v2, "appid")
	v2["azp"], v2["aud"], v2["iss"] = azuremonitor.ActionGroupsAppID, azuremonitortest.AppID, "https://login.microsoftonline.com/"+azuremonitortest.TenantID+"/v2.0"
	if err := door.Check(ctx, entra.Sign(v2)); err != nil {
		t.Fatalf("a good v2 token was refused: %v", err)
	}
	bad := map[string]func(map[string]any){
		"another tenant":   func(c map[string]any) { c["tid"] = "tenant-2" },
		"another issuer":   func(c map[string]any) { c["iss"] = "https://sts.windows.net/tenant-2/" },
		"another app":      func(c map[string]any) { c["aud"] = "api://other" },
		"another client":   func(c map[string]any) { c["appid"] = "someone-else" },
		"without the role": func(c map[string]any) { c["roles"] = []string{"Other"} },
		"no roles":         func(c map[string]any) { delete(c, "roles") },
		"expired":          func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
	}
	for name, change := range bad {
		claims := entra.Claims()
		change(claims)
		if err := door.Check(ctx, entra.Sign(claims)); !errors.Is(err, azuremonitor.ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	forged := azuremonitortest.New(t)
	if err := door.Check(ctx, forged.Sign(entra.Claims())); !errors.Is(err, azuremonitor.ErrRefused) {
		t.Errorf("a token signed by another key: %v", err)
	}
	if !azuremonitor.LooksSigned(entra.Sign(entra.Claims())) || azuremonitor.LooksSigned("Bearer dsp_abc") || azuremonitor.LooksSigned("") {
		t.Error("LooksSigned tells a JWT from a desk token")
	}
}
