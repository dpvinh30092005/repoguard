// Package platformtest provides a scripted fake HTTP API for client tests.
package platformtest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type Request struct {
	Method, URI, Body string
	Header            http.Header
}

// Server answers "METHOD /path-prefix" routes with canned JSON and records every request.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	routes   []route
	Requests []Request
}

type route struct {
	method, prefix string
	status         int
	body           string
}

func New(t *testing.T) *Server {
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// On registers a response for requests whose method matches and URI starts with prefix.
// Later registrations win, so tests can override defaults.
func (s *Server) On(method, prefix string, status int, body string) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = append([]route{{method, prefix, status, body}}, s.routes...)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.Requests = append(s.Requests, Request{Method: r.Method, URI: r.RequestURI, Body: string(b), Header: r.Header.Clone()})
	routes := s.routes
	s.mu.Unlock()
	for _, rt := range routes {
		if rt.method == r.Method && strings.HasPrefix(r.RequestURI, rt.prefix) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rt.status)
			_, _ = io.WriteString(w, rt.body)
			return
		}
	}
	http.Error(w, `{"message":"no route"}`, http.StatusNotFound)
}

// Find returns recorded requests matching method and URI prefix.
func (s *Server) Find(method, prefix string) []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Request
	for _, r := range s.Requests {
		if r.Method == method && strings.HasPrefix(r.URI, prefix) {
			out = append(out, r)
		}
	}
	return out
}
