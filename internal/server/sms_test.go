package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/sms"
)

// smsSent records texts instead of sending them.
type smsSent struct{ to []string }

func (s *smsSent) SendPerson(_ context.Context, out notice.Outgoing) (int, error) {
	s.to = append(s.to, fmt.Sprintf("%s:%d", out.Person.Phone, out.MessageID))
	return 202, nil
}

func TestTextsAnswerPagesAndReportsAreKept(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.desk.Store
	box := h.desk.Notices()
	sent := &smsSent{}
	box.UseMedium(notice.SMS, sent)
	st.SavePerson(ctx, notice.Person{Email: member, Phone: "+15555550100", Rules: notice.Rules{notice.WeightPage: {{Medium: notice.SMS}}}})
	st.SaveChain(ctx, notice.Chain{Slug: "oncall", Name: "On call", Steps: []notice.Step{{Targets: []notice.Target{{Person: member}}, Wait: 5}, {Targets: []notice.Target{{Person: admin}}, Wait: 5}}})
	if _, err := box.Accept(ctx, notice.Event{Producer: "grafana", Kind: "alert.disk", Key: "d1", Title: "Disk full", To: []notice.Target{{Chain: "oncall"}}}); err != nil {
		t.Fatal(err)
	}
	box.SendDue(ctx)
	if len(sent.to) != 1 || !strings.HasPrefix(sent.to[0], "+15555550100:") {
		t.Fatalf("texted %v", sent.to)
	}
	grid := func(body string) *httptest.ResponseRecorder {
		return h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/sms", strings.NewReader(body)), shared))
	}
	rec := grid(`[{"id":"v","eventType":"Microsoft.EventGrid.SubscriptionValidationEvent","data":{"validationCode":"code-1"}}]`)
	var v map[string]string
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 200 || v["validationResponse"] != "code-1" {
		t.Fatalf("validation %d %v", rec.Code, v)
	}
	if rec := h.do(h.api, httptest.NewRequest(http.MethodPost, "/v1/sms", strings.NewReader(`[]`))); rec.Code != http.StatusUnauthorized {
		t.Errorf("without the token: %d", rec.Code)
	}
	var id int64
	fmt.Sscanf(sent.to[0], "+15555550100:%d", &id)
	if rec := grid(fmt.Sprintf(`[{"id":"r","eventType":"Microsoft.Communication.SMSDeliveryReportReceived","data":{"messageId":"m","to":"+15555550100","deliveryStatus":"Delivered","deliveryStatusDetails":"No error.","tag":%q}}]`, sms.Tag(id))); rec.Code != 200 {
		t.Fatalf("report %d", rec.Code)
	}
	if m, _ := st.Message(ctx, id); m.DeliveredAt == nil || m.Receipt != "Delivered: No error." {
		t.Errorf("receipt %+v", m)
	}
	if rec := grid(`[{"id":"s","eventType":"Microsoft.Communication.SMSReceived","data":{"messageId":"in","from":"+15555559999","to":"+15555550199","message":"ack"}}]`); rec.Code != 200 {
		t.Fatalf("stranger %d", rec.Code)
	}
	if p, _ := st.Page(ctx, 1); p.State != notice.PageActive {
		t.Fatal("a number nobody has acknowledged a page")
	}
	if rec := grid(`[{"id":"s","eventType":"Microsoft.Communication.SMSReceived","data":{"messageId":"in","from":"+15555550100","to":"+15555550199","message":"ACK"}}]`); rec.Code != 200 {
		t.Fatalf("reply %d", rec.Code)
	}
	if p, _ := st.Page(ctx, 1); p.State != notice.PageAcknowledged || p.AcknowledgedBy != member {
		t.Errorf("the reply took the page: %+v", p)
	}
	page := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/pages/1", nil), member, "User"))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "on the device") {
		t.Errorf("the page shows the receipt: %d", page.Code)
	}
	if m, _ := st.Message(ctx, id); m.OpenedAt == nil || m.OpenedAt.After(time.Now()) {
		t.Errorf("viewing the page marks it opened: %+v", m)
	}
}
