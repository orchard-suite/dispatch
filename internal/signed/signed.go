// Package signed checks a JWT that another service signed with a key from
// its published OpenID key set: the token's shape, its RS256 signature, and
// its time. What the claims must say is the caller's to check.
package signed

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrRefused is a token that is not well signed, or not in its time.
var ErrRefused = errors.New("the token is refused")

// Skew is how far a token's times may be off the clock.
const Skew = 5 * time.Minute

// refresh is how long a key set is kept before it is read again.
const refresh = 24 * time.Hour

// Claims are a checked token's claims.
type Claims map[string]any

// Text is a string claim, empty when it is missing or not a string.
func (c Claims) Text(name string) string {
	v, _ := c[name].(string)
	return v
}

// For says whether the token's audience names want.
func (c Claims) For(want string) bool {
	switch v := c["aud"].(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if a == want {
				return true
			}
		}
	}
	return false
}

// Has says whether the list claim name holds want, such as a role in roles.
func (c Claims) Has(name, want string) bool {
	list, _ := c[name].([]any)
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Keys is a service's published signing keys, read from its OpenID
// document and read again a day later, or as soon as a token names a key
// it does not hold.
type Keys struct {
	openID string
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// NewKeys reads keys from the OpenID document at openID through client,
// on the clock now.
func NewKeys(openID string, client *http.Client, now func() time.Time) *Keys {
	return &Keys{openID: openID, client: client, now: now}
}

// UseClient reads keys through c, for a test against its own TLS server.
func (k *Keys) UseClient(c *http.Client) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.client = c
}

// Check reads an Authorization header's bearer token and returns its
// claims when one of the keys signed it with RS256 and the clock is
// within its exp and nbf. A refusal wraps ErrRefused; a failure to read
// the keys does not.
func (k *Keys) Check(ctx context.Context, authorization string) (Claims, error) {
	raw, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: no bearer token", ErrRefused)
	}
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a JWT", ErrRefused)
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &head); err != nil || head.Alg != "RS256" || head.Kid == "" {
		return nil, fmt.Errorf("%w: the token is not RS256 with a key id", ErrRefused)
	}
	key, err := k.find(ctx, head.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable signature", ErrRefused)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, fmt.Errorf("%w: bad signature", ErrRefused)
	}
	var claims Claims
	if err := decodePart(parts[1], &claims); err != nil {
		return nil, fmt.Errorf("%w: unreadable claims", ErrRefused)
	}
	if !within(claims, k.now()) {
		return nil, fmt.Errorf("%w: outside its time", ErrRefused)
	}
	return claims, nil
}

func decodePart(part string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func within(claims Claims, now time.Time) bool {
	exp, ok := claims["exp"].(float64)
	if !ok || now.After(time.Unix(int64(exp), 0).Add(Skew)) {
		return false
	}
	if nbf, ok := claims["nbf"].(float64); ok && now.Add(Skew).Before(time.Unix(int64(nbf), 0)) {
		return false
	}
	return true
}

func (k *Keys) find(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key, ok := k.keys[kid]; ok && k.now().Sub(k.fetched) < refresh {
		return key, nil
	}
	// A key it does not hold is read for at most once a minute, so forged key ids cannot make it fetch on every request.
	if k.keys == nil || k.now().Sub(k.fetched) >= time.Minute {
		if err := k.fetch(ctx); err != nil {
			if key, ok := k.keys[kid]; ok {
				return key, nil
			}
			return nil, err
		}
	}
	if key, ok := k.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: unknown signing key", ErrRefused)
}

func (k *Keys) fetch(ctx context.Context) error {
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := k.get(ctx, k.openID, &doc); err != nil || doc.JWKSURI == "" {
		return fmt.Errorf("the OpenID document: %v", err)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := k.get(ctx, doc.JWKSURI, &set); err != nil {
		return fmt.Errorf("the signing keys: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, j := range set.Keys {
		if j.Kty != "RSA" || j.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(j.N)
		e, err2 := base64.RawURLEncoding.DecodeString(j.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			continue
		}
		keys[j.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("the key set holds no RSA keys")
	}
	k.keys, k.fetched = keys, k.now()
	return nil
}

func (k *Keys) get(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	res, err := k.client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return uerr.Err
		}
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("answered %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
}
