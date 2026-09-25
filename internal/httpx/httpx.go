// Package httpx holds the small HTTP/JSON and environment helpers shared by
// every Spillway component.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// WriteJSON writes v as a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// Error writes a JSON error body.
func Error(w http.ResponseWriter, code int, msg string) {
	WriteJSON(w, code, map[string]string{"error": msg})
}

// ReadJSON decodes a JSON request body (max 1 MiB). An empty body is not an error.
func ReadJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

// RequireToken protects mutating requests with a shared bearer token.
// GET/HEAD requests and an empty token are always allowed.
func RequireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-Spillway-Token")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if got != token {
			Error(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Client is a tiny JSON-over-HTTP client used between components.
type Client struct {
	HTTP  *http.Client
	Token string
}

// NewClient returns a client with the given per-request timeout.
func NewClient(timeout time.Duration, token string) *Client {
	return &Client{HTTP: &http.Client{Timeout: timeout}, Token: token}
}

// StatusError is returned for non-2xx responses.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("http %d: %s", e.Code, e.Body) }

// Do sends body (if non-nil) as JSON and decodes the response into out (if non-nil).
func (c *Client) Do(ctx context.Context, method, url string, body, out any) error {
	return c.DoWithHeaders(ctx, method, url, nil, body, out)
}

// DoWithHeaders is Do with extra request headers.
func (c *Client) DoWithHeaders(ctx context.Context, method, url string, headers map[string]string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("X-Spillway-Token", c.Token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Env returns the environment variable or def when unset/empty.
func Env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// EnvInt parses an int environment variable.
func EnvInt(key string, def int) int {
	if v := Env(key, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("config: invalid int %s=%q, using %d", key, v, def)
	}
	return def
}

// EnvFloat parses a float environment variable.
func EnvFloat(key string, def float64) float64 {
	if v := Env(key, ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		log.Printf("config: invalid float %s=%q, using %v", key, v, def)
	}
	return def
}

// EnvBool parses a bool environment variable.
func EnvBool(key string, def bool) bool {
	if v := Env(key, ""); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		log.Printf("config: invalid bool %s=%q, using %v", key, v, def)
	}
	return def
}

// EnvDur parses a duration environment variable ("5s", "300ms").
func EnvDur(key string, def time.Duration) time.Duration {
	if v := Env(key, ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("config: invalid duration %s=%q, using %v", key, v, def)
	}
	return def
}

// Serve runs an HTTP server until ctx is cancelled.
func Serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
