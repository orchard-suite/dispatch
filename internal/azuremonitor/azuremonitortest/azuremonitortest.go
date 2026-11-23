// Package azuremonitortest is a stand-in for Entra, for tests: a tenant's
// OpenID document and keys on one TLS server, and the tokens it signs for
// an action group's secure webhook.
package azuremonitortest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/azuremonitor"
)

const (
	// AppID is the desk's app the tokens are for; TenantID its tenant.
	AppID    = "desk-app"
	TenantID = "tenant-1"
)

// Entra is the stand-in.
type Entra struct {
	*httptest.Server
	key *rsa.PrivateKey
}

// New starts the stand-in.
func New(t *testing.T) *Entra {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e := &Entra{key: key}
	e.Server = httptest.NewTLSServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.Close)
	return e
}

// Door is a door that reads this stand-in's keys.
func (e *Entra) Door(t *testing.T) *azuremonitor.Door {
	t.Helper()
	d, err := azuremonitor.NewDoor(azuremonitor.DoorConfig{AppID: AppID, TenantID: TenantID, LoginURL: e.URL, Client: e.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Claims are a good token's claims: a v1 token Azure Monitor's app got for
// the desk's app, holding the role.
func (e *Entra) Claims() map[string]any {
	return map[string]any{
		"iss": "https://sts.windows.net/" + TenantID + "/", "tid": TenantID, "aud": "api://" + AppID,
		"appid": azuremonitor.ActionGroupsAppID, "roles": []string{azuremonitor.Role},
		"exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
	}
}

// Sign makes the Authorization header the action group sends.
func (e *Entra) Sign(claims map[string]any) string {
	head := enc(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	body := enc(claims)
	digest := sha256.Sum256([]byte(head + "." + body))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, e.key, crypto.SHA256, digest[:])
	return "Bearer " + head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func enc(v any) string {
	raw, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (e *Entra) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/" + TenantID + "/v2.0/.well-known/openid-configuration":
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": e.URL + "/keys"})
	case "/keys":
		n := base64.RawURLEncoding.EncodeToString(e.key.N.Bytes())
		x := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(e.key.E)).Bytes())
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "n": n, "e": x}}})
	default:
		http.NotFound(w, r)
	}
}
