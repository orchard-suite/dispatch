package seal

import (
	"errors"
	"testing"
)

func TestSealOpens(t *testing.T) {
	s, err := New("key one")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.Seal("https://example.org/hook?sig=abc")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Open(sealed); err != nil || got != "https://example.org/hook?sig=abc" {
		t.Fatalf("opened %q, %v", got, err)
	}
	other, _ := New("key two")
	if _, err := other.Open(sealed); err == nil {
		t.Error("another key opened it")
	}
	none, _ := New("")
	if _, err := none.Seal("x"); !errors.Is(err, ErrNoKey) {
		t.Errorf("sealed without a key: %v", err)
	}
}
