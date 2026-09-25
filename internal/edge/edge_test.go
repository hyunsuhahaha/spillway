package edge

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testConfig(specs ...BackendSpec) Config {
	return Config{
		HoldTimeout: 2 * time.Second, HealthInterval: 50 * time.Millisecond,
		HealthTimeout: 200 * time.Millisecond, HealthFails: 2, HealthPath: "/healthz", Initial: specs,
	}
}

func named(name string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			io.WriteString(w, "ok")
			return
		}
		body, _ := io.ReadAll(r.Body)
		io.WriteString(w, name+":"+r.URL.Path+":"+string(body))
	}))
}

func markAllHealthy(s *Server) {
	s.mu.Lock()
	for _, b := range s.backends {
		b.healthy = true
	}
	s.mu.Unlock()
}

func TestConfigFromEnvParsesBackends(t *testing.T) {
	t.Setenv("EDGE_BACKENDS", "local=http://local-app:8080@local:2, cloud=https://x.run.app@cloud")
	cfg := ConfigFromEnv()
	if len(cfg.Initial) != 2 {
		t.Fatalf("want 2 backends, got %d", len(cfg.Initial))
	}
	l, c := cfg.Initial[0], cfg.Initial[1]
	if l.Name != "local" || l.URL != "http://local-app:8080" || l.Group != "local" || l.Units != 2 || !l.Enabled {
		t.Fatalf("bad local spec %+v", l)
	}
	if c.Group != "cloud" || c.Units != 1 {
		t.Fatalf("bad cloud spec %+v", c)
	}
}

func TestWeightedSplitFollowsUnits(t *testing.T) {
	a, b := named("a"), named("b")
	defer a.Close()
	defer b.Close()
	s := New(testConfig(
		BackendSpec{Name: "a", URL: a.URL, Group: "local", Units: 1, Enabled: true},
		BackendSpec{Name: "b", URL: b.URL, Group: "cloud", Units: 3, Enabled: true},
	))
	markAllHealthy(s)
	counts := map[string]int{}
	for i := 0; i < 800; i++ {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
		counts[rec.Header().Get("X-Spillway-Backend")]++
	}
	// Sequential requests have no in-flight load, so the split follows the
	// weights: roughly 1:3 (expected 200 / 600).
	if counts["a"] < 140 || counts["a"] > 260 {
		t.Fatalf("unexpected split %v", counts)
	}
}

func TestProxiesPathBodyAndHeaders(t *testing.T) {
	a := named("a")
	defer a.Close()
	s := New(testConfig(BackendSpec{Name: "a", URL: a.URL, Group: "local", Units: 1, Enabled: true}))
	markAllHealthy(s)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/api/entries", strings.NewReader("hello")))
	if got := rec.Body.String(); got != "a:/api/entries:hello" {
		t.Fatalf("body = %q", got)
	}
	if rec.Header().Get("X-Spillway-Group") != "local" {
		t.Fatalf("missing group header")
	}
}

func TestHoldsWhilePausedThenDelivers(t *testing.T) {
	a := named("a")
	defer a.Close()
	s := New(testConfig(BackendSpec{Name: "a", URL: a.URL, Group: "local", Units: 1, Enabled: true}))
	markAllHealthy(s)
	s.mu.Lock()
	s.paused = true
	s.notifyLocked()
	s.mu.Unlock()
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.mu.Lock()
		s.paused = false
		s.notifyLocked()
		s.mu.Unlock()
	}()
	start := time.Now()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/held", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatalf("request was not held")
	}
}

func TestNoBackendReturns503AfterHold(t *testing.T) {
	cfg := testConfig()
	cfg.HoldTimeout = 200 * time.Millisecond
	s := New(cfg)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestDialFailureRetriesOnOtherBackendAndMarksDead(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	deadURL := "http://" + ln.Addr().String()
	ln.Close() // nothing listens here any more
	b := named("b")
	defer b.Close()
	s := New(testConfig(
		BackendSpec{Name: "dead", URL: deadURL, Group: "local", Units: 1000, Enabled: true},
		BackendSpec{Name: "b", URL: b.URL, Group: "cloud", Units: 1, Enabled: true},
	))
	markAllHealthy(s)
	rec := httptest.NewRecorder()
	// POST without Idempotency-Key: still retried because a dial error means
	// the request never reached the dead backend.
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/w", strings.NewReader("x")))
	if rec.Code != 200 || rec.Header().Get("X-Spillway-Backend") != "b" {
		t.Fatalf("status %d backend %q", rec.Code, rec.Header().Get("X-Spillway-Backend"))
	}
	s.mu.Lock()
	healthy := s.backends["dead"].healthy
	s.mu.Unlock()
	if healthy {
		t.Fatalf("dead backend should be marked unhealthy")
	}
}

func TestHealthLoopMarksBackends(t *testing.T) {
	a := named("a")
	s := New(testConfig(BackendSpec{Name: "a", URL: a.URL, Group: "local", Units: 1, Enabled: true}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.healthLoop(ctx)
	waitHealthy := func(want bool) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			s.mu.Lock()
			h := s.backends["a"].healthy
			s.mu.Unlock()
			if h == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("backend healthy != %v", want)
	}
	waitHealthy(true)
	a.Close()
	waitHealthy(false)
}

func TestSetBackendsKeepsRuntimeState(t *testing.T) {
	a := named("a")
	defer a.Close()
	s := New(testConfig(BackendSpec{Name: "a", URL: a.URL, Group: "local", Units: 1, Enabled: true}))
	markAllHealthy(s)
	s.setBackends([]BackendSpec{{Name: "a", URL: a.URL, Group: "local", Units: 5, Enabled: false}})
	s.mu.Lock()
	b := s.backends["a"]
	s.mu.Unlock()
	if !b.healthy || b.spec.Units != 5 || b.spec.Enabled {
		t.Fatalf("spec not updated in place: %+v healthy=%v", b.spec, b.healthy)
	}
	s.setBackends(nil)
	s.mu.Lock()
	n := len(s.backends)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("backend not removed")
	}
}

func TestSingleJoin(t *testing.T) {
	cases := [][3]string{{"", "/a", "/a"}, {"/", "/a", "/a"}, {"/base/", "/a", "/base/a"}, {"/base", "a", "/base/a"}, {"/base", "/a", "/base/a"}}
	for _, c := range cases {
		if got := singleJoin(c[0], c[1]); got != c[2] {
			t.Errorf("singleJoin(%q,%q)=%q want %q", c[0], c[1], got, c[2])
		}
	}
}
