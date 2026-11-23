// Package teamstest is a stand-in for Microsoft's bot service, for tests:
// the OpenID document and keys, the sign-in endpoint, the Bot Connector,
// and the token store, all on one TLS server.
package teamstest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/teams"
)

const (
	AppID    = "bot-app"
	TenantID = "tenant-1"
	// Code is the sign-in code the token store redeems; SSO is the Teams
	// token it exchanges. Either gives PersonToken.
	Code        = "good-code"
	SSO         = "sso-token"
	PersonToken = "person-token"
	botToken    = "bot-token"
)

// Service is the stand-in. Posts and Replies are what the bot sent.
type Service struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu      sync.Mutex
	Posts   []map[string]any
	Replies []map[string]any
	// SignedIn holds the users the token store already has a token for.
	SignedIn map[string]bool
	// Roster is every team's members.
	Roster []teams.Member
}

// New starts the stand-in.
func New(t *testing.T) *Service {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{key: key, SignedIn: map[string]bool{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Bot is a bot that signs in and speaks to the stand-in.
func (s *Service) Bot(t *testing.T) *teams.Bot {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("workload-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := teams.New(teams.Config{AppID: AppID, TenantID: TenantID, TokenFile: file, Connection: "orrery",
		OpenIDURL: s.URL + "/openid", LoginURL: s.URL, TokenStore: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	b.UseClient(s.Client())
	return b
}

// Sign makes the token the bot service sends with an activity.
func (s *Service) Sign(claims map[string]any) string {
	head := enc(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	body := enc(claims)
	digest := sha256.Sum256([]byte(head + "." + body))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// Claims are a good token's claims for an activity from the stand-in.
func (s *Service) Claims() map[string]any {
	return map[string]any{"iss": "https://api.botframework.com", "aud": AppID, "serviceurl": s.URL, "exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix()}
}

func enc(v any) string {
	raw, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.URL.Path == "/openid":
		reply(200, map[string]string{"jwks_uri": s.URL + "/keys"})
		return
	case r.URL.Path == "/keys":
		pub := s.key.PublicKey
		reply(200, map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())}}})
		return
	case r.URL.Path == "/"+TenantID+"/oauth2/v2.0/token":
		_ = r.ParseForm()
		if r.Form.Get("client_assertion") != "workload-token" || r.Form.Get("client_id") != AppID {
			reply(401, map[string]string{"error_description": "bad assertion"})
			return
		}
		reply(200, map[string]any{"access_token": botToken, "expires_in": 3600})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+botToken {
		reply(401, map[string]string{"error": "the bot did not sign in"})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/v3/conversations":
		s.Posts = append(s.Posts, body)
		reply(201, map[string]string{"id": "conv-1", "activityId": "act-1"})
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v3/conversations/"):
		s.Replies = append(s.Replies, body)
		reply(200, map[string]string{"id": "reply-1"})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v3/conversations/") && strings.HasSuffix(r.URL.Path, "/pagedmembers"):
		reply(200, map[string]any{"members": s.Roster})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v3/teams/"):
		reply(200, map[string]any{"conversations": []map[string]any{{"id": "19:general", "name": nil}, {"id": "19:approvals", "name": "Approvals"}}})
	case r.URL.Path == "/api/usertoken/GetToken":
		if q.Get("code") == Code || s.SignedIn[q.Get("userId")] {
			s.SignedIn[q.Get("userId")] = true
			reply(200, map[string]string{"token": PersonToken})
			return
		}
		reply(404, map[string]string{"error": "no token"})
	case r.URL.Path == "/api/usertoken/exchange":
		if body["token"] == SSO {
			// The token store keeps what it exchanged, as the real one does.
			s.SignedIn[q.Get("userId")] = true
			reply(200, map[string]string{"token": PersonToken})
			return
		}
		reply(412, map[string]string{"error": "cannot exchange"})
	case r.URL.Path == "/api/botsignin/GetSignInResource":
		reply(200, map[string]any{"signInLink": "https://sign.in/link", "tokenExchangeResource": map[string]string{"id": "x", "uri": "api://botid-" + AppID}})
	default:
		reply(404, map[string]string{"error": "no such route"})
	}
}

// Sent is a copy of what the bot posted.
func (s *Service) Sent() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any{}, s.Posts...)
}

// Said is a copy of the bot's replies.
func (s *Service) Said() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any{}, s.Replies...)
}
