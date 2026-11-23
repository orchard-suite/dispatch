package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

var letter = notice.Mail(notice.Event{Producer: "vantage", Title: "Drift in Shop", Text: "Two boxes changed.", Severity: "warning",
	Links: []notice.Link{{Title: "Open", URL: "https://vantage.example.org/d/1"}}, At: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}, "https://dispatch.example.org/pages/3")

func TestGraphSignsInWithTheWorkloadTokenAndSendsAsTheMailbox(t *testing.T) {
	var mu sync.Mutex
	var tokens, sends int
	var sent map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/tenant-1/oauth2/v2.0/token":
			_ = r.ParseForm()
			tokens++
			if r.Form.Get("client_assertion") != "workload-token" || r.Form.Get("client_id") != "mail-app" || r.Form.Get("client_secret") != "" || r.Form.Get("scope") != "https://graph.microsoft.com/.default" {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error_description":"bad assertion"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"graph-token","expires_in":3600}`))
		case r.URL.Path == "/v1.0/users/alerts@example.com/sendMail":
			sends++
			if r.Header.Get("Authorization") != "Bearer graph-token" {
				w.WriteHeader(401)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &sent)
			if sends == 2 {
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"error":{"code":"ErrorAccessDenied","message":"Access is denied.\nmore"}}`))
				return
			}
			w.WriteHeader(202)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(file, []byte("workload-token\n"), 0o600)
	g, err := NewGraph(GraphConfig{AppID: "mail-app", TenantID: "tenant-1", TokenFile: file, From: "alerts@example.com", LoginURL: srv.URL, GraphURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	g.UseClient(srv.Client())
	status, err := g.Send(context.Background(), "one@example.com", letter)
	if err != nil || status != 202 {
		t.Fatalf("send: %d %v", status, err)
	}
	msg := sent["message"].(map[string]any)
	if msg["subject"] != "[warning] Drift in Shop" || sent["saveToSentItems"] != false {
		t.Errorf("sent %v", sent)
	}
	body := msg["body"].(map[string]any)
	if body["contentType"] != "HTML" || !strings.Contains(body["content"].(string), "https://dispatch.example.org/pages/3") {
		t.Errorf("body %v", body)
	}
	if to := msg["toRecipients"].([]any)[0].(map[string]any)["emailAddress"].(map[string]any)["address"]; to != "one@example.com" {
		t.Errorf("to %v", to)
	}
	status, err = g.Send(context.Background(), "two@example.com", letter)
	if status != 403 || err == nil || err.Error() != "Graph answered 403 ErrorAccessDenied: Access is denied." {
		t.Errorf("refusal: %d %v", status, err)
	}
	if !notice.Permanent(status) {
		t.Error("a 403 is tried again")
	}
	if tokens != 1 {
		t.Errorf("signed in %d times, want the token kept", tokens)
	}
}

// fakeSMTP is an SMTP server that offers STARTTLS and records the conversation.
type fakeSMTP struct {
	ln       net.Listener
	cert     tls.Certificate
	mu       sync.Mutex
	lines    []string
	data     string
	rejectTo string
}

func newFakeSMTP(t *testing.T) (*fakeSMTP, *tls.Config) {
	t.Helper()
	certSrv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(certSrv.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSMTP{ln: ln, cert: certSrv.TLS.Certificates[0]}
	go f.serve()
	client := certSrv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	client.ServerName = "example.com"
	return f, client
}

func (f *fakeSMTP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.talk(conn)
	}
}

func (f *fakeSMTP) talk(conn net.Conn) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), conn
	say := func(s string) { _, _ = io.WriteString(w, s+"\r\n") }
	say("220 fake ready")
	tlsOn := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.mu.Lock()
		f.lines = append(f.lines, line)
		f.mu.Unlock()
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch {
		case verb == "EHLO":
			if tlsOn {
				say("250-fake\r\n250 AUTH PLAIN")
			} else {
				say("250-fake\r\n250 STARTTLS")
			}
		case verb == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			if err := tc.Handshake(); err != nil {
				return
			}
			r, w, tlsOn = bufio.NewReader(tc), tc, true
		case verb == "AUTH":
			if line != "AUTH PLAIN "+base64.StdEncoding.EncodeToString([]byte("\x00user@example.com\x00app pass")) {
				say("535 bad credentials")
				continue
			}
			say("235 ok")
		case verb == "MAIL":
			say("250 ok")
		case verb == "QUIT":
			say("221 bye")
			return
		case verb == "RCPT":
			if f.rejectTo != "" && strings.Contains(line, f.rejectTo) {
				say("550 no such mailbox")
				continue
			}
			say("250 ok")
		case verb == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		default:
			say("500 what")
		}
	}
}

func TestSMTPUpgradesSignsInAndSendsBothParts(t *testing.T) {
	f, tlsCfg := newFakeSMTP(t)
	s, err := NewSMTP("smtp://user%40example.com:app%20pass@"+f.ln.Addr().String(), "alerts@example.com")
	if err != nil {
		t.Fatal(err)
	}
	s.TLS = tlsCfg
	status, err := s.Send(context.Background(), "one@example.com", letter)
	if err != nil || status != 202 {
		f.mu.Lock()
		t.Fatalf("send: %d %v\n%s", status, err, strings.Join(f.lines, "\n"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	joined := strings.Join(f.lines, "\n")
	if !strings.Contains(joined, "STARTTLS") || !strings.Contains(joined, "MAIL FROM:<alerts@example.com>") || !strings.Contains(joined, "RCPT TO:<one@example.com>") {
		t.Errorf("conversation:\n%s", joined)
	}
	if !strings.Contains(f.data, "text/plain") || !strings.Contains(f.data, "text/html") || !strings.Contains(f.data, "Acknowledge: https://dispatch.example.org/pages/3") {
		t.Errorf("message:\n%s", f.data)
	}
}

func TestSMTPRefusalIsPermanentAndKeepsThePasswordOut(t *testing.T) {
	f, tlsCfg := newFakeSMTP(t)
	f.rejectTo = "nobody@"
	s, _ := NewSMTP("smtp://user%40example.com:app%20pass@"+f.ln.Addr().String(), "alerts@example.com")
	s.TLS = tlsCfg
	status, err := s.Send(context.Background(), "nobody@example.com", letter)
	if status != 400 || err == nil || !notice.Permanent(status) {
		t.Fatalf("refusal: %d %v", status, err)
	}
	if strings.Contains(err.Error(), "app pass") {
		t.Error("the error carries the password")
	}
	if _, err := NewSMTP("smtp://user:se cret@%zz", "a@example.com"); err == nil || strings.Contains(err.Error(), "se cret") {
		t.Errorf("a bad URL: %v", err)
	}
}

func TestNewPicksOneTransport(t *testing.T) {
	if s, err := New(Config{}); s != nil || err != nil {
		t.Errorf("no mail: %v %v", s, err)
	}
	if _, err := New(Config{From: "a@example.com", GraphAppID: "x", SMTPURL: "smtp://h"}); err == nil {
		t.Error("both transports accepted")
	}
	if _, err := New(Config{SMTPURL: "smtp://h"}); err == nil {
		t.Error("no MAIL_FROM accepted")
	}
	if s, err := New(Config{From: "a@example.com", SMTPURL: "smtps://u:p@smtp.example.com"}); err != nil || s.Kind() != "smtp" {
		t.Errorf("smtp: %v", err)
	}
}
