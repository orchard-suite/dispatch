package notice

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries a webhook post's signatures.
const SignatureHeader = "X-Dispatch-Signature"

// NewSecret is a fresh signing secret.
func NewSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "whsec_" + hex.EncodeToString(b)
}

func mac(secret string, at int64, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(h, "%d.", at)
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Sign is the signature header for a post of body at a time: "t=<unix
// seconds>", then one "v1=" HMAC-SHA256 of "<t>.<body>" per secret, the
// current one first and the one being retired after it, so a receiver
// that still holds the old secret keeps verifying while it changes over.
func Sign(secrets []string, at time.Time, body []byte) string {
	t := at.Unix()
	parts := []string{"t=" + strconv.FormatInt(t, 10)}
	for _, s := range secrets {
		if s != "" {
			parts = append(parts, "v1="+mac(s, t, body))
		}
	}
	return strings.Join(parts, ",")
}

// Verify is what a receiver does: the header holds for the body under
// the secret, within tolerance of now.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) bool {
	var t int64 = -1
	var sigs []string
	for _, p := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		switch k {
		case "t":
			t, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if t < 0 {
		return false
	}
	if d := now.Sub(time.Unix(t, 0)); d > tolerance || d < -tolerance {
		return false
	}
	want := mac(secret, t, body)
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return true
		}
	}
	return false
}
