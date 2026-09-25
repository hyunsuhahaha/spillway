// Package edge implements the Spillway edge proxy: a single public entry point
// that spreads traffic across the local site and burst instances by weight,
// health-checks every backend, and can hold requests while the control plane
// switches sites so users see a short delay instead of errors.
package edge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"spillway/internal/httpx"
	"spillway/internal/metrics"
)

// BackendSpec is the desired configuration of one backend.
type BackendSpec struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Group   string `json:"group"`
	Units   int    `json:"units"`
	Enabled bool   `json:"enabled"`
}

// BackendView is a backend plus its runtime state.
type BackendView struct {
	BackendSpec
	Healthy   bool      `json:"healthy"`
	LastErr   string    `json:"last_err,omitempty"`
	LastCheck time.Time `json:"last_check"`
	Inflight  int64     `json:"inflight"`
	Served    int64     `json:"served"`
}

// StateView is returned by the admin API.
type StateView struct {
	Paused      bool          `json:"paused"`
	PauseReason string        `json:"pause_reason,omitempty"`
	Held        int64         `json:"held"`
	Backends    []BackendView `json:"backends"`
	// Stats/Groups measure what users see (including time held during a
	// switch). Upstream* measure only backend service time and drive burst
	// decisions, so a failover pause is never mistaken for overload.
	Stats          metrics.Stats            `json:"stats"`
	Groups         map[string]metrics.Stats `json:"groups"`
	Upstream       metrics.Stats            `json:"upstream"`
	UpstreamGroups map[string]metrics.Stats `json:"upstream_groups"`
}

// Config configures the edge.
type Config struct {
	Listen         string
	AdminListen    string
	Token          string
	HoldTimeout    time.Duration
	HealthInterval time.Duration
	HealthTimeout  time.Duration
	HealthFails    int
	HealthPath     string
	Initial        []BackendSpec
}

type backend struct {
	spec      BackendSpec
	target    *url.URL
	healthy   bool
	fails     int
	lastErr   string
	lastCheck time.Time
	inflight  atomic.Int64
	served    atomic.Int64
	// gen is cancelled when the backend is marked unhealthy so requests
	// stuck on a dead backend (e.g. a network partition) fail fast.
	gen    context.Context
	cancel context.CancelFunc
}

// Server is the edge proxy.
type Server struct {
	cfg       Config
	mu        sync.Mutex
	backends  map[string]*backend
	paused    bool
	reason    string
	changed   chan struct{}
	held      atomic.Int64
	win       *metrics.Window
	upWin     *metrics.Window
	transport *http.Transport
	health    *http.Client
}

// New builds an edge server.
func New(cfg Config) *Server {
	dialer := &net.Dialer{Timeout: 1500 * time.Millisecond, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 25 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	s := &Server{
		cfg:       cfg,
		backends:  map[string]*backend{},
		changed:   make(chan struct{}),
		win:       metrics.NewWindow(6 * time.Second),
		upWin:     metrics.NewWindow(6 * time.Second),
		transport: tr,
		health:    &http.Client{Timeout: cfg.HealthTimeout, Transport: tr},
	}
	s.setBackends(cfg.Initial)
	return s
}

// ConfigFromEnv reads the edge configuration from the environment.
// EDGE_BACKENDS format: "name=url[@group[:units]],..." e.g. "local=http://local-app:8080@local:2".
func ConfigFromEnv() Config {
	cfg := Config{
		Listen:         httpx.Env("EDGE_LISTEN", ":8080"),
		AdminListen:    httpx.Env("EDGE_ADMIN_LISTEN", ":8081"),
		Token:          httpx.Env("SPILLWAY_TOKEN", ""),
		HoldTimeout:    httpx.EnvDur("EDGE_HOLD_TIMEOUT", 30*time.Second),
		HealthInterval: httpx.EnvDur("EDGE_HEALTH_INTERVAL", time.Second),
		HealthTimeout:  httpx.EnvDur("EDGE_HEALTH_TIMEOUT", 800*time.Millisecond),
		HealthFails:    httpx.EnvInt("EDGE_HEALTH_FAILS", 2),
		HealthPath:     httpx.Env("EDGE_HEALTH_PATH", "/healthz"),
	}
	for _, item := range strings.Split(httpx.Env("EDGE_BACKENDS", ""), ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, rest, ok := strings.Cut(item, "=")
		if !ok {
			log.Printf("edge: ignoring malformed backend %q", item)
			continue
		}
		spec := BackendSpec{Name: name, Group: name, Units: 1, Enabled: true}
		u, meta, _ := strings.Cut(rest, "@")
		spec.URL = u
		if meta != "" {
			g, units, _ := strings.Cut(meta, ":")
			spec.Group = g
			if units != "" {
				var n int
				for _, ch := range units {
					if ch >= '0' && ch <= '9' {
						n = n*10 + int(ch-'0')
					}
				}
				spec.Units = n
			}
		}
		cfg.Initial = append(cfg.Initial, spec)
	}
	return cfg
}

// Run starts the public listener, the admin listener and the health checker.
func (s *Server) Run(ctx context.Context) error {
	go s.healthLoop(ctx)
	errc := make(chan error, 2)
	go func() { errc <- httpx.Serve(ctx, s.cfg.Listen, s) }()
	go func() { errc <- httpx.Serve(ctx, s.cfg.AdminListen, s.AdminHandler()) }()
	log.Printf("edge: public %s, admin %s", s.cfg.Listen, s.cfg.AdminListen)
	return <-errc
}

// ---------------------------------------------------------------- backends

func (s *Server) setBackends(specs []BackendSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, sp := range specs {
		if sp.Name == "" || sp.URL == "" {
			continue
		}
		u, err := url.Parse(sp.URL)
		if err != nil || u.Host == "" {
			log.Printf("edge: invalid backend url %q", sp.URL)
			continue
		}
		seen[sp.Name] = true
		if b, ok := s.backends[sp.Name]; ok && b.spec.URL == sp.URL {
			b.spec = sp
			continue
		} else if ok {
			b.cancel()
		}
		gen, cancel := context.WithCancel(context.Background())
		s.backends[sp.Name] = &backend{spec: sp, target: u, gen: gen, cancel: cancel}
	}
	for name, b := range s.backends {
		if !seen[name] {
			b.cancel()
			delete(s.backends, name)
		}
	}
	s.notifyLocked()
}

func (s *Server) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// pickLocked chooses a routable backend with "power of two choices": two
// candidates are drawn by weight (Units) and the one with fewer in-flight
// requests per unit wins (ties keep the first draw). Weights set the long-run split; the in-flight check
// keeps a saturated backend (a busy on-prem server) from building a queue.
func (s *Server) pickLocked(exclude map[string]bool) *backend {
	total := 0
	cands := make([]*backend, 0, len(s.backends))
	for _, b := range s.backends {
		if !b.spec.Enabled || !b.healthy || b.spec.Units <= 0 || exclude[b.spec.Name] {
			continue
		}
		cands = append(cands, b)
		total += b.spec.Units
	}
	if total == 0 {
		return nil
	}
	draw := func() *backend {
		n := rand.IntN(total)
		for _, b := range cands {
			n -= b.spec.Units
			if n < 0 {
				return b
			}
		}
		return cands[len(cands)-1]
	}
	a := draw()
	if len(cands) == 1 {
		return a
	}
	b := draw()
	// Utilisation per unit of capacity. With no load both are 0 and the
	// weighted first draw wins, so idle traffic follows the configured split.
	load := func(x *backend) float64 { return float64(x.inflight.Load()) / float64(x.spec.Units) }
	if load(b) < load(a) {
		return b
	}
	return a
}

// waitPick blocks until a backend is routable, the edge is resumed, or the deadline passes.
func (s *Server) waitPick(ctx context.Context, exclude map[string]bool, deadline time.Time) *backend {
	counted := false
	defer func() {
		if counted {
			s.held.Add(-1)
		}
	}()
	for {
		s.mu.Lock()
		if !s.paused {
			if b := s.pickLocked(exclude); b != nil {
				s.mu.Unlock()
				return b
			}
		}
		ch := s.changed
		s.mu.Unlock()
		if !counted {
			counted = true
			s.held.Add(1)
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil
		}
		if wait > 250*time.Millisecond {
			wait = 250 * time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil
		}
		t.Stop()
	}
}

func (s *Server) markHealth(b *backend, ok bool, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, exists := s.backends[b.spec.Name]; !exists || cur != b {
		return
	}
	b.lastCheck = time.Now()
	if ok {
		b.fails = 0
		b.lastErr = ""
		if !b.healthy {
			b.healthy = true
			log.Printf("edge: backend %s healthy", b.spec.Name)
			s.notifyLocked()
		}
		return
	}
	b.fails++
	b.lastErr = errMsg
	if b.healthy && b.fails >= s.cfg.HealthFails {
		b.healthy = false
		log.Printf("edge: backend %s unhealthy: %s", b.spec.Name, errMsg)
		// Abort requests stuck on this backend and start a fresh generation.
		b.cancel()
		b.gen, b.cancel = context.WithCancel(context.Background())
		s.transport.CloseIdleConnections()
		s.notifyLocked()
	}
}

// markDead is the passive health check: a connection-level failure while
// proxying immediately takes the backend out of rotation.
func (s *Server) markDead(b *backend, err error) {
	s.mu.Lock()
	b.fails = s.cfg.HealthFails - 1
	s.mu.Unlock()
	s.markHealth(b, false, "proxy: "+err.Error())
}

func (s *Server) healthLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.HealthInterval)
	defer t.Stop()
	for {
		s.checkAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) checkAll(ctx context.Context) {
	s.mu.Lock()
	list := make([]*backend, 0, len(s.backends))
	for _, b := range s.backends {
		list = append(list, b)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, b := range list {
		wg.Add(1)
		go func(b *backend) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, s.cfg.HealthTimeout)
			defer cancel()
			u := *b.target
			u.Path = singleJoin(b.target.Path, s.cfg.HealthPath)
			req, _ := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), nil)
			resp, err := s.health.Do(req)
			if err != nil {
				s.markHealth(b, false, err.Error())
				return
			}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				s.markHealth(b, true, "")
			} else {
				s.markHealth(b, false, resp.Status)
			}
		}(b)
	}
	wg.Wait()
}

// ---------------------------------------------------------------- proxying

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_spillway/health" {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
		return
	}
	start := time.Now()
	var body []byte
	if r.Body != nil && r.ContentLength != 0 {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
	}
	retrySafe := r.Method == http.MethodGet || r.Method == http.MethodHead ||
		r.Method == http.MethodOptions || r.Header.Get("Idempotency-Key") != ""

	deadline := start.Add(s.cfg.HoldTimeout)
	exclude := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		b := s.waitPick(r.Context(), exclude, deadline)
		if b == nil {
			break
		}
		upStart := time.Now()
		resp, err := s.forward(r, b, body)
		if err != nil {
			if r.Context().Err() != nil {
				return // client went away
			}
			lastErr = err
			exclude[b.spec.Name] = true
			if isConnErr(err) {
				s.markDead(b, err)
			}
			if isDialErr(err) || retrySafe {
				continue
			}
			break
		}
		s.copyResponse(w, resp, b)
		b.served.Add(1)
		now := time.Now()
		ok := resp.StatusCode < 500
		s.win.Add(metrics.Sample{At: now, Dur: now.Sub(start), OK: ok, Group: b.spec.Group})
		s.upWin.Add(metrics.Sample{At: now, Dur: now.Sub(upStart), OK: ok, Group: b.spec.Group})
		return
	}
	s.win.Add(metrics.Sample{At: time.Now(), Dur: time.Since(start), OK: false, Group: "none"})
	if lastErr != nil {
		http.Error(w, "spillway edge: upstream error: "+lastErr.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Retry-After", "2")
	http.Error(w, "spillway edge: no healthy backend available", http.StatusServiceUnavailable)
}

func (s *Server) forward(r *http.Request, b *backend, body []byte) (*http.Response, error) {
	s.mu.Lock()
	gen := b.gen
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(gen, cancel)

	out := r.Clone(ctx)
	out.RequestURI = ""
	out.URL.Scheme = b.target.Scheme
	out.URL.Host = b.target.Host
	out.URL.Path = singleJoin(b.target.Path, r.URL.Path)
	out.URL.RawPath = ""
	out.Host = b.target.Host
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	if len(body) == 0 {
		out.Body = http.NoBody
	}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
			ip = prior + ", " + ip
		}
		out.Header.Set("X-Forwarded-For", ip)
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	out.Header.Set("X-Forwarded-Proto", proto)
	out.Header.Set("X-Forwarded-Host", r.Host)

	b.inflight.Add(1)
	resp, err := s.transport.RoundTrip(out)
	b.inflight.Add(-1)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: func() { stop(); cancel() }}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel func()
}

func (c *cancelBody) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func (s *Server) copyResponse(w http.ResponseWriter, resp *http.Response, b *backend) {
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Spillway-Backend", b.spec.Name)
	w.Header().Set("X-Spillway-Group", b.spec.Group)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func isDialErr(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func isConnErr(err error) bool {
	if isDialErr(err) || errors.Is(err, context.Canceled) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func singleJoin(a, b string) string {
	switch {
	case a == "" || a == "/":
		if b == "" {
			return "/"
		}
		return b
	case strings.HasSuffix(a, "/") && strings.HasPrefix(b, "/"):
		return a + b[1:]
	case !strings.HasSuffix(a, "/") && !strings.HasPrefix(b, "/"):
		return a + "/" + b
	}
	return a + b
}

// ---------------------------------------------------------------- admin API

// State returns a snapshot of the edge.
func (s *Server) State() StateView {
	s.mu.Lock()
	v := StateView{Paused: s.paused, PauseReason: s.reason, Held: s.held.Load()}
	groups := map[string]bool{}
	for _, b := range s.backends {
		v.Backends = append(v.Backends, BackendView{
			BackendSpec: b.spec, Healthy: b.healthy, LastErr: b.lastErr, LastCheck: b.lastCheck,
			Inflight: b.inflight.Load(), Served: b.served.Load(),
		})
		groups[b.spec.Group] = true
	}
	s.mu.Unlock()
	sort.Slice(v.Backends, func(i, j int) bool { return v.Backends[i].Name < v.Backends[j].Name })
	v.Stats = s.win.Stats("")
	v.Upstream = s.upWin.Stats("")
	v.Groups = map[string]metrics.Stats{}
	v.UpstreamGroups = map[string]metrics.Stats{}
	for g := range groups {
		v.Groups[g] = s.win.Stats(g)
		v.UpstreamGroups[g] = s.upWin.Stats(g)
	}
	v.Groups["none"] = s.win.Stats("none")
	return v
}

// AdminHandler exposes the control API used by the control plane.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, s.State())
	})
	mux.HandleFunc("PUT /backends", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Backends []BackendSpec `json:"backends"`
		}
		if err := httpx.ReadJSON(r, &req); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		s.setBackends(req.Backends)
		httpx.WriteJSON(w, http.StatusOK, s.State())
	})
	mux.HandleFunc("PATCH /backends/{name}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Units   *int  `json:"units"`
			Enabled *bool `json:"enabled"`
		}
		if err := httpx.ReadJSON(r, &req); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		s.mu.Lock()
		b, ok := s.backends[r.PathValue("name")]
		if ok {
			if req.Units != nil {
				b.spec.Units = *req.Units
			}
			if req.Enabled != nil {
				b.spec.Enabled = *req.Enabled
			}
			s.notifyLocked()
		}
		s.mu.Unlock()
		if !ok {
			httpx.Error(w, http.StatusNotFound, "unknown backend")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, s.State())
	})
	mux.HandleFunc("POST /pause", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Reason string `json:"reason"`
		}
		_ = httpx.ReadJSON(r, &req)
		s.mu.Lock()
		s.paused, s.reason = true, req.Reason
		s.notifyLocked()
		s.mu.Unlock()
		log.Printf("edge: paused (%s)", req.Reason)
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"paused": true})
	})
	mux.HandleFunc("POST /resume", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paused, s.reason = false, ""
		s.notifyLocked()
		s.mu.Unlock()
		log.Printf("edge: resumed")
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"paused": false})
	})
	return httpx.RequireToken(s.cfg.Token, mux)
}
