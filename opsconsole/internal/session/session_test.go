package session

import (
	"strings"
	"testing"
	"time"
)

func validSession() Session {
	return Session{
		Username: "sobhan", DisplayName: "Sobhan",
		IssuedAt: time.Now(), ExpiresAt: time.Now().Add(12 * time.Hour),
	}
}

func TestNewSigner_RejectsShortSecret(t *testing.T) {
	if _, err := NewSigner("too-short"); err == nil {
		t.Fatal("want an error for a secret under 32 bytes, got nil")
	}
}

func TestSignThenVerify_RoundTrips(t *testing.T) {
	sg, err := NewSigner(strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	want := validSession()
	cookie, err := sg.Sign(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sg.Verify(cookie)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Username != want.Username || got.DisplayName != want.DisplayName {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestVerify_RejectsTamperedPayload(t *testing.T) {
	sg, err := NewSigner(strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := sg.Sign(validSession())
	if err != nil {
		t.Fatal(err)
	}
	tampered := cookie[:len(cookie)-10] + "AAAAAAAAAA"
	if _, err := sg.Verify(tampered); err != ErrInvalidSignature {
		t.Fatalf("Verify(tampered) = %v, want ErrInvalidSignature", err)
	}
}

func TestVerify_RejectsWrongSecret(t *testing.T) {
	sg1, err := NewSigner(strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	sg2, err := NewSigner(strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := sg1.Sign(validSession())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sg2.Verify(cookie); err != ErrInvalidSignature {
		t.Fatalf("Verify with wrong secret = %v, want ErrInvalidSignature", err)
	}
}

func TestVerify_RejectsExpired(t *testing.T) {
	sg, err := NewSigner(strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	s := validSession()
	s.ExpiresAt = time.Now().Add(-1 * time.Minute)
	cookie, err := sg.Sign(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sg.Verify(cookie); err != ErrExpired {
		t.Fatalf("Verify(expired) = %v, want ErrExpired", err)
	}
}

func TestVerify_RejectsMalformedCookie(t *testing.T) {
	sg, err := NewSigner(strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "no-dot-here", ".", "abc."} {
		if _, err := sg.Verify(bad); err == nil {
			t.Errorf("Verify(%q) = nil error, want a rejection", bad)
		}
	}
}
