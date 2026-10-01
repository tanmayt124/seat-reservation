// Package auth verifies JWTs and puts the caller's identity into the request
// context. Handlers take the user id only from here, never from the request
// body, query string or headers.
package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"

	issuer = "seat-reservation"
	leeway = 5 * time.Second
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrExpiredToken = errors.New("token expired")

	userIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

type Identity struct {
	UserID string
	Role   Role
}

func (i Identity) IsAdmin() bool { return i.Role == RoleAdmin }

type claims struct {
	Role Role `json:"role"`
	jwt.RegisteredClaims
}

// Authenticator signs and verifies HS256 tokens with one shared secret.
type Authenticator struct {
	secret []byte
	parser *jwt.Parser
	now    func() time.Time
}

func New(secret string) *Authenticator {
	return &Authenticator{
		secret: []byte(secret),
		// Pinning the method blocks alg=none and algorithm-confusion tokens.
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithIssuer(issuer),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			jwt.WithLeeway(leeway),
		),
		now: time.Now,
	}
}

func ValidUserID(id string) bool { return userIDPattern.MatchString(id) }

func ValidRole(r Role) bool { return r == RoleUser || r == RoleAdmin }

// Issue mints a token. Used by the test-only token endpoint and by tests.
func (a *Authenticator) Issue(userID string, role Role, ttl time.Duration) (string, time.Time, error) {
	if !ValidUserID(userID) {
		return "", time.Time{}, fmt.Errorf("invalid user id %q", userID)
	}
	if !ValidRole(role) {
		return "", time.Time{}, fmt.Errorf("invalid role %q", role)
	}
	now := a.now()
	exp := now.Add(ttl)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	signed, err := tok.SignedString(a.secret)
	return signed, exp, err
}

// Verify checks signature, algorithm, issuer and expiry, then validates the
// subject and role claims.
func (a *Authenticator) Verify(raw string) (Identity, error) {
	var c claims
	_, err := a.parser.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return a.secret, nil })
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Identity{}, ErrExpiredToken
		}
		return Identity{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !ValidUserID(c.Subject) {
		return Identity{}, fmt.Errorf("%w: bad subject", ErrInvalidToken)
	}
	if !ValidRole(c.Role) {
		return Identity{}, fmt.Errorf("%w: bad role", ErrInvalidToken)
	}
	return Identity{UserID: c.Subject, Role: c.Role}, nil
}

type ctxKey struct{}

func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the identity set by the auth middleware.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}
