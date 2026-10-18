package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/platform"
)

// TokenSource yields a token able to act on repositories of an installation.
type TokenSource interface {
	Token(ctx context.Context, installationID int64) (string, error)
}

// StaticToken is a personal access token (simple setups, GitHub Enterprise without Apps).
type StaticToken string

func (t StaticToken) Token(context.Context, int64) (string, error) { return string(t), nil }

// AppAuth implements GitHub App authentication: a short-lived JWT signed with
// the App's private key is exchanged for per-installation tokens (1h), cached
// until shortly before expiry.
type AppAuth struct {
	AppID int64
	key   *rsa.PrivateKey
	rest  *platform.REST
	now   func() time.Time
	mu    sync.Mutex
	cache map[int64]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

func NewAppAuth(appID int64, privateKeyPEM []byte, apiURL string, hc *http.Client) (*AppAuth, error) {
	key, err := parseKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	a := &AppAuth{AppID: appID, key: key, now: time.Now, cache: map[int64]cachedToken{}}
	a.rest = &platform.REST{HTTP: hc, BaseURL: apiURL, UserAgent: userAgent, Authorize: func(r *http.Request) error {
		jwt, err := a.JWT()
		if err != nil {
			return err
		}
		r.Header.Set("Authorization", "Bearer "+jwt)
		return nil
	}}
	return a, nil
}

func parseKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("github app private key: not PEM encoded")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app private key: not RSA")
	}
	return rk, nil
}

// JWT returns an App JWT valid for ~9 minutes (GitHub allows 10; iat is
// backdated 60s to tolerate clock skew).
func (a *AppAuth) JWT() (string, error) {
	now := a.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": fmt.Sprint(a.AppID),
	})
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (a *AppAuth) Token(ctx context.Context, installationID int64) (string, error) {
	if installationID == 0 {
		return "", errors.New("github app: event has no installation id")
	}
	a.mu.Lock()
	if c, ok := a.cache[installationID]; ok && a.now().Before(c.expires.Add(-5*time.Minute)) {
		a.mu.Unlock()
		return c.token, nil
	}
	a.mu.Unlock()

	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := a.rest.Do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), nil, &resp); err != nil {
		return "", fmt.Errorf("github app: installation token: %w", err)
	}
	a.mu.Lock()
	a.cache[installationID] = cachedToken{token: resp.Token, expires: resp.ExpiresAt}
	a.mu.Unlock()
	return resp.Token, nil
}

// Installations lists the installations of the App (used to discover repos).
func (a *AppAuth) Installations(ctx context.Context) ([]int64, error) {
	var ids []int64
	for page := 1; ; page++ {
		var resp []struct {
			ID int64 `json:"id"`
		}
		if _, err := a.rest.Do(ctx, http.MethodGet, fmt.Sprintf("/app/installations?per_page=100&page=%d", page), nil, &resp); err != nil {
			return nil, err
		}
		for _, i := range resp {
			ids = append(ids, i.ID)
		}
		if len(resp) < 100 {
			return ids, nil
		}
	}
}
