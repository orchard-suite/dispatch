// Package seal keeps a secret unreadable at rest: AES-256-GCM under a key
// made from SECRET_KEY. A sealed value carries its own nonce.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

var ErrNoKey = errors.New("SECRET_KEY is not set, so no secret can be kept")

type Sealer struct {
	aead cipher.AEAD
}

// New makes a sealer from key. An empty key gives a sealer that refuses
// every secret, so a desk without one still starts.
func New(key string) (*Sealer, error) {
	if key == "" {
		return &Sealer{}, nil
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Keeps says whether secrets can be sealed.
func (s *Sealer) Keeps() bool { return s.aead != nil }

func (s *Sealer) Seal(plain string) (string, error) {
	if s.aead == nil {
		return "", ErrNoKey
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (s *Sealer) Open(sealed string) (string, error) {
	if s.aead == nil {
		return "", ErrNoKey
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", errors.New("the sealed value is damaged")
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("the sealed value does not open with this SECRET_KEY")
	}
	return string(plain), nil
}
