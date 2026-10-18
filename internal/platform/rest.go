package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// REST is a minimal JSON-over-HTTP helper shared by the platform clients.
// We call a few dozen endpoints; a thin layer beats two large SDKs.
type REST struct {
	HTTP      *http.Client
	BaseURL   string
	UserAgent string
	// Authorize sets credentials on a request; it receives the repo's context
	// (e.g. GitHub App installation) via the request's context.
	Authorize func(req *http.Request) error
}

func NewHTTPClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Do sends in (if non-nil) as JSON and decodes the response into out (if non-nil).
func (r *REST) Do(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	raw, hdr, err := r.do(ctx, method, path, in, "application/json")
	if err != nil {
		return hdr, err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return hdr, fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return hdr, nil
}

// Raw returns the response body as-is, sending the given Accept header.
func (r *REST) Raw(ctx context.Context, path, accept string) ([]byte, error) {
	raw, _, err := r.do(ctx, http.MethodGet, path, nil, accept)
	return raw, err
}

func (r *REST) do(ctx context.Context, method, path string, in any, accept string) ([]byte, http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.BaseURL+path, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", r.UserAgent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.Authorize != nil {
		if err := r.Authorize(req); err != nil {
			return nil, nil, err
		}
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, nil, retryableNetErr{err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return nil, resp.Header, retryableNetErr{err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.Header, &APIError{Method: method, URL: path, Status: resp.StatusCode, Body: string(raw)}
	}
	return raw, resp.Header, nil
}

// Is lets callers write errors.Is(err, platform.ErrNotFound).
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

type retryableNetErr struct{ error }

func (retryableNetErr) Retryable() bool { return true }
func (e retryableNetErr) Unwrap() error { return e.error }
