package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const secret = "test-secret-test-secret-test-secret-123"

func TestIssueAndVerify(t *testing.T) {
	a := New(secret)
	tok, _, err := a.Issue("alice", RoleUser, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != "alice" || id.Role != RoleUser || id.IsAdmin() {
		t.Fatalf("unexpected identity %+v", id)
	}
}

func TestVerifyRejects(t *testing.T) {
	a := New(secret)
	other := New("a-completely-different-secret-value-123")
	good, _, _ := a.Issue("alice", RoleUser, time.Hour)
	wrongSig, _, _ := other.Issue("alice", RoleUser, time.Hour)

	expired := New(secret)
	expired.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	expiredTok, _, _ := expired.Issue("alice", RoleUser, time.Hour)

	sign := func(m jwt.SigningMethod, key any, c jwt.Claims) string {
		s, err := jwt.NewWithClaims(m, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	base := func(role Role, sub string) claims {
		return claims{Role: role, RegisteredClaims: jwt.RegisteredClaims{
			Subject: sub, Issuer: issuer,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		}}
	}
	noExp := base(RoleUser, "alice")
	noExp.ExpiresAt = nil
	wrongIssuer := base(RoleUser, "alice")
	wrongIssuer.Issuer = "someone-else"

	cases := map[string]struct {
		token string
		want  error
	}{
		"empty":            {"", ErrInvalidToken},
		"garbage":          {"not.a.jwt", ErrInvalidToken},
		"wrong signature":  {wrongSig, ErrInvalidToken},
		"tampered payload": {tamper(good), ErrInvalidToken},
		"expired":          {expiredTok, ErrExpiredToken},
		"alg none":         {sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, base(RoleAdmin, "mallory")), ErrInvalidToken},
		"alg HS512":        {sign(jwt.SigningMethodHS512, []byte(secret), base(RoleUser, "alice")), ErrInvalidToken},
		"missing exp":      {sign(jwt.SigningMethodHS256, []byte(secret), noExp), ErrInvalidToken},
		"wrong issuer":     {sign(jwt.SigningMethodHS256, []byte(secret), wrongIssuer), ErrInvalidToken},
		"missing role":     {sign(jwt.SigningMethodHS256, []byte(secret), base("", "alice")), ErrInvalidToken},
		"unknown role":     {sign(jwt.SigningMethodHS256, []byte(secret), base("root", "alice")), ErrInvalidToken},
		"bad subject":      {sign(jwt.SigningMethodHS256, []byte(secret), base(RoleUser, "a b")), ErrInvalidToken},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := a.Verify(c.token); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// tamper swaps the payload for one claiming admin, keeping the old signature.
func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	parts[1] = "eyJyb2xlIjoiYWRtaW4iLCJzdWIiOiJtYWxsb3J5In0"
	return strings.Join(parts, ".")
}
