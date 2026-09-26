package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"spillway/internal/cloud"
	"spillway/internal/edge"
	"spillway/internal/httpx"
	"spillway/internal/metrics"
	"spillway/internal/siteagent"
)

func testController() *Controller {
	c := New(Config{LocalAppURL: "http://local-app:8080", LocalUnits: 2, P95High: 250, P95Low: 120, MinRPS: 2,
		Sustain: 3 * time.Second, StepCooldown: 8 * time.Second, ScaleInAfter: 15 * time.Second, EvacAfter: 3 * time.Second,
		AppDownAfter: 3 * time.Second, BurstMin: 2, BurstMax: 6, EvacMin: 2})
	c.provSt = []cloud.Status{{Provider: "sim", Endpoints: []cloud.Endpoint{{Name: "b0", URL: "http://b0:8080", Units: 1}, {Name: "b1", URL: "http://b1:8080", Units: 1}}}}
	return c
}

func TestCloudProviderRequiresControlToken(t *testing.T) {
	t.Setenv("CLOUD_PROVIDERS", "cloudrun")
	t.Setenv("GCP_PROJECT", "test-project")
	for _, token := range []string{"", "   ", "change-me-long-random-token", " change-me-long-random-token "} {
		t.Setenv("SPILLWAY_TOKEN", token)
		if _, err := ConfigFromEnv(); err == nil {
			t.Fatalf("cloud provider accepted unsafe token %q", token)
		}
	}
	t.Setenv("SPILLWAY_TOKEN", "test-only-unique-secret-123456")
	if _, err := ConfigFromEnv(); err != nil {
		t.Fatalf("cloud provider rejected configured token: %v", err)
	}
	t.Setenv("CLOUD_PROVIDERS", "docker")
	t.Setenv("SPILLWAY_TOKEN", "")
	if _, err := ConfigFromEnv(); err != nil {
		t.Fatalf("local simulation should remain usable without a token: %v", err)
	}
}

func TestControlRejectsUnauthenticatedMutation(t *testing.T) {
	c := testController()
	c.cfg.Token = "test-only-secret"
	req := httptest.NewRequest(http.MethodPost, "/api/__auth-check", nil)
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST returned %d, want 401", rec.Code)
	}
	req.Header.Set("X-Spillway-Token", c.cfg.Token)
	rec = httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("authenticated POST should reach router, got %d", rec.Code)
	}
}

func TestDashboardDoesNotPersistControlToken(t *testing.T) {
	if strings.Contains(string(dashboardHTML), "localStorage.setItem") || strings.Contains(string(dashboardHTML), "localStorage.getItem") {
		t.Fatal("dashboard must not persist or reload the control token")
	}
}

// The mock models the two database roles, not just HTTP responses: a failed
// response to promote can still mean PostgreSQL was already promoted.
func TestFailbackCutoverFailures(t *testing.T) {
	for _, tc := range []struct {
		name, failAt, wantMode string
		wantCloudFenced        bool
		wantLocalRole          string
		wantResumed            bool
	}{
		{"success", "", string(ModeNormal), false, "primary", true},
		{"edge pause response lost", "edge-pause", string(ModeEvacuated), false, "standby", true},
		{"router pause response lost", "router-pause", string(ModeEvacuated), false, "standby", true},
		{"router kill fails", "router-kill", string(ModeEvacuated), false, "standby", true},
		{"fence response lost", "cloud-fence", string(ModeEvacuated), false, "standby", true},
		{"promote response lost", "local-promote", string(ModeSwitchingBack), true, "primary", false},
		{"router target fails", "router-target", string(ModeSwitchingBack), true, "primary", false},
		{"router target mismatch", "router-target-mismatch", string(ModeSwitchingBack), true, "primary", false},
		{"router resume response lost", "router-resume", string(ModeSwitchingBack), true, "primary", false},
		{"edge routes fail", "edge-backends", string(ModeSwitchingBack), true, "primary", false},
		{"edge resume response lost", "edge-resume", string(ModeSwitchingBack), true, "primary", false},
		{"reprotect fails", "cloud-rebuild", string(ModeNormal), true, "primary", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			localRole, cloudRole, cloudFenced := "standby", "primary", false
			edgePaused, routerPaused, routerTarget := false, false, "cloud:5432"
			var calls []string
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method != http.MethodGet {
					calls = append(calls, r.Method+" "+r.URL.Path)
				}
				write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
				switch r.URL.Path {
				case "/local/status":
					write(siteagent.Status{Site: "local", State: "running", Running: true, Role: localRole, Streaming: localRole == "standby", LSNBytes: 100})
				case "/cloud/status":
					write(siteagent.Status{Site: "cloud", State: "running", Running: true, Role: cloudRole, Fenced: cloudFenced, LSNBytes: 100,
						Replicas: []siteagent.Replica{{Name: "local", LagBytes: 0}}})
				case "/edge/state":
					write(edge.StateView{Paused: edgePaused, Backends: []edge.BackendView{{BackendSpec: edge.BackendSpec{Name: "local", Group: "local"}, Healthy: true}}})
				case "/router/status":
					write(map[string]any{"paused": routerPaused, "target": routerTarget})
				case "/probe/status":
					write(map[string]any{})
				case "/local/rebuild":
					write(map[string]any{})
				case "/edge/backends":
					if tc.failAt == "edge-backends" {
						http.Error(w, "routes failed", http.StatusInternalServerError)
						return
					}
					write(map[string]any{})
				case "/router/kill":
					if tc.failAt == "router-kill" {
						http.Error(w, "kill failed", http.StatusInternalServerError)
						return
					}
					write(map[string]any{})
				case "/router/target":
					var body struct {
						Addr string `json:"addr"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if tc.failAt != "router-target-mismatch" {
						routerTarget = body.Addr
					}
					if tc.failAt == "router-target" {
						http.Error(w, "response lost after target", http.StatusInternalServerError)
						return
					}
					write(map[string]any{})
				case "/cloud/fence":
					var body struct {
						On bool `json:"on"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					cloudFenced = body.On
					if tc.failAt == "cloud-fence" && body.On {
						http.Error(w, "response lost after fence", http.StatusInternalServerError)
						return
					}
					write(map[string]any{})
				case "/local/promote":
					localRole = "primary"
					if tc.failAt == "local-promote" {
						http.Error(w, "response lost after promote", http.StatusInternalServerError)
						return
					}
					write(map[string]any{})
				case "/cloud/rebuild":
					if tc.failAt == "cloud-rebuild" {
						http.Error(w, "rebuild failed", http.StatusInternalServerError)
						return
					}
					cloudRole, cloudFenced = "standby", false
					write(map[string]any{})
				case "/edge/pause":
					edgePaused = true
					if tc.failAt == "edge-pause" {
						http.Error(w, "response lost after pause", http.StatusInternalServerError)
						return
					}
					write(map[string]any{})
				case "/edge/resume":
					edgePaused = false
					if tc.failAt == "edge-resume" {
						http.Error(w, "response lost after resume", http.StatusInternalServerError)
						return
					}
					write(map[string]any{})
				case "/router/pause":
					routerPaused = true
					if tc.failAt == "router-pause" {
						http.Error(w, "response lost after pause", http.StatusInternalServerError)
						return
					}
					write(map[string]any{})
				case "/router/resume":
					routerPaused = false
					if tc.failAt == "router-resume" {
						http.Error(w, "response lost after resume", http.StatusInternalServerError)
						return
					}
					write(map[string]any{})
				default:
					http.NotFound(w, r)
				}
			})
			srv := httptest.NewServer(h)
			defer srv.Close()
			c := testController()
			c.cfg.EdgeAdmin, c.cfg.DBRouterAdmin = srv.URL+"/edge", srv.URL+"/router"
			c.cfg.LocalAgent, c.cfg.CloudAgent, c.cfg.ProbeURL = srv.URL+"/local", srv.URL+"/cloud", srv.URL+"/probe"
			c.cfg.RouterLocalPG, c.cfg.RouterCloudPG = "local:5432", "cloud:5432"
			c.cfg.CloudPGForLocal, c.cfg.LocalPGForCloud = "cloud:5432", "local:5432"
			c.mode, c.localEnabled, c.target = ModeEvacuated, false, 2
			c.localSt = &siteagent.Status{Site: "local", State: "running", Running: true, Role: "primary"}
			c.failback(context.Background())

			mu.Lock()
			defer mu.Unlock()
			if string(c.mode) != tc.wantMode || cloudFenced != tc.wantCloudFenced || localRole != tc.wantLocalRole {
				t.Fatalf("mode=%s cloudFenced=%t localRole=%s, calls=%v", c.mode, cloudFenced, localRole, calls)
			}
			if edgePaused == tc.wantResumed || routerPaused == tc.wantResumed {
				t.Fatalf("edgePaused=%t routerPaused=%t, want resumed=%t", edgePaused, routerPaused, tc.wantResumed)
			}
			fence, promote := -1, -1
			for i, call := range calls {
				if call == "POST /cloud/fence" && fence < 0 {
					fence = i
				}
				if call == "POST /local/promote" {
					promote = i
				}
			}
			if promote >= 0 && (fence < 0 || fence >= promote) {
				t.Fatalf("old primary must be fenced before new promotion: %v", calls)
			}
		})
	}
}

func TestReconcileKeepsInterruptedFailbackPaused(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	c := testController()
	c.cfg.EdgeAdmin, c.cfg.DBRouterAdmin = srv.URL+"/edge", srv.URL+"/router"
	c.cloudSt = &siteagent.Status{Running: true, Role: "primary", Fenced: true}
	c.localSt = &siteagent.Status{Running: true, Role: "primary"}
	c.reconcile(t.Context())
	if c.mode != ModeSwitchingBack || c.localEnabled {
		t.Fatalf("interrupted failback resumed as %s, local enabled=%t", c.mode, c.localEnabled)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != "POST /edge/pause" || calls[1] != "POST /router/pause" {
		t.Fatalf("restart must hold both entry points, calls=%v", calls)
	}
}

func TestDesiredBackendsZeroUnitsWhenNotBursting(t *testing.T) {
	c := testController()
	specs := c.desiredBackends()
	if len(specs) != 3 || specs[0].Name != "local" || specs[0].Units != 2 || !specs[0].Enabled {
		t.Fatalf("specs %+v", specs)
	}
	for _, s := range specs[1:] {
		if s.Units != 0 || s.Group != "cloud" {
			t.Fatalf("idle burst backend should get no traffic: %+v", s)
		}
	}
	c.target = 2
	c.localEnabled = false
	specs = c.desiredBackends()
	if specs[0].Enabled || specs[1].Units != 1 || specs[2].Units != 1 {
		t.Fatalf("bursting specs %+v", specs)
	}
}

func TestIdleCloudRunDoesNotReachEdgeHealthChecker(t *testing.T) {
	c := testController()
	c.provSt = []cloud.Status{{Kind: "cloudrun", Endpoints: []cloud.Endpoint{{Name: "run", URL: "https://app.run.app", Units: 2}}}}
	if got := c.desiredBackends(); len(got) != 1 {
		t.Fatalf("zero-min Cloud Run should be absent from edge: %+v", got)
	}
	c.cfg.WarmMin = 1
	if got := c.desiredBackends(); len(got) != 2 || got[1].Units != 0 {
		t.Fatalf("warm Cloud Run should be health-checked but not routed: %+v", got)
	}
	c.cfg.WarmMin = 0
	c.target = 2
	if got := c.desiredBackends(); len(got) != 2 || got[1].Units != 2 {
		t.Fatalf("burst Cloud Run should be routed: %+v", got)
	}
	c.target = 0
	c.provSt[0].Desired = 2 // keep draining in-flight requests before scale-down
	if got := c.desiredBackends(); len(got) != 2 || got[1].Units != 0 {
		t.Fatalf("draining Cloud Run should remain configured: %+v", got)
	}
}

func TestHeldNeedsTheConditionToHaveStarted(t *testing.T) {
	now := time.Now()
	if held(time.Time{}, now, 0) {
		t.Fatal("a condition that never started must not count as held, even for d=0")
	}
	if !held(now.Add(-3*time.Second), now, 3*time.Second) || held(now.Add(-time.Second), now, 3*time.Second) {
		t.Fatal("held threshold wrong")
	}
}

func TestSinceAndDur(t *testing.T) {
	now := time.Now()
	if !since(time.Time{}, false, now).IsZero() {
		t.Fatal("false condition must reset")
	}
	s := since(time.Time{}, true, now)
	if !s.Equal(now) || !since(s, true, now.Add(time.Second)).Equal(now) {
		t.Fatal("since must keep the first time the condition held")
	}
	if dur(time.Time{}, now) != 0 || dur(now.Add(-2*time.Second), now) != 2*time.Second {
		t.Fatal("dur wrong")
	}
}

func TestProtectionStates(t *testing.T) {
	c := testController()
	if p := c.protection(); p.State != "none" {
		t.Fatalf("no local status: %+v", p)
	}
	c.localSt = &siteagent.Status{Role: "primary", Replicas: []siteagent.Replica{{Name: "cloud", State: "streaming", Sync: "async", LagBytes: 128}}}
	if p := c.protection(); p.State != "ok" || p.LagBytes != 128 {
		t.Fatalf("streaming replica: %+v", p)
	}
	c.localSt.Replicas = nil
	if p := c.protection(); p.State != "degraded" {
		t.Fatalf("no replica: %+v", p)
	}
	c.mode = ModeEvacuated
	if p := c.protection(); p.State != "degraded" {
		t.Fatalf("evacuated: %+v", p)
	}
}

func TestDecideEntersBurstOnlyAfterSustainedHighLatency(t *testing.T) {
	c := testController()
	c.cfg.Providers = []cloud.Provider{noopProvider{}}
	c.localSt = &siteagent.Status{Running: true, Role: "primary"}
	high := metrics.Stats{Count: 100, RPS: 50, P95: 900}
	c.edgeState = &edge.StateView{
		Stats: high, Upstream: high,
		Backends:       []edge.BackendView{{BackendSpec: edge.BackendSpec{Name: "local", Group: "local", Units: 2, Enabled: true}, Healthy: true}},
		UpstreamGroups: map[string]metrics.Stats{"local": high},
	}
	c.decide(t.Context())
	if c.mode != ModeNormal {
		t.Fatalf("burst must wait for the sustain period, got %s", c.mode)
	}
	c.highSince = time.Now().Add(-4 * time.Second)
	c.decide(t.Context())
	if c.mode != ModeBurst || c.target != 2 {
		t.Fatalf("expected burst with 2 instances, got %s/%d", c.mode, c.target)
	}
	if c.localCap != 50 {
		t.Fatalf("local capacity should be measured while saturated, got %v", c.localCap)
	}
}

func TestDecideIgnoresLatencyDuringQuietPeriod(t *testing.T) {
	c := testController()
	c.cfg.Providers = []cloud.Provider{noopProvider{}}
	c.localSt = &siteagent.Status{Running: true, Role: "primary"}
	high := metrics.Stats{Count: 100, RPS: 50, P95: 900}
	c.edgeState = &edge.StateView{Stats: high, Upstream: high,
		Backends: []edge.BackendView{{BackendSpec: edge.BackendSpec{Name: "local", Group: "local", Units: 2, Enabled: true}, Healthy: true}}}
	c.quietUntil = time.Now().Add(time.Minute)
	c.highSince = time.Now().Add(-10 * time.Second)
	c.decide(t.Context())
	if c.mode != ModeNormal {
		t.Fatalf("no burst during the post-switch quiet period, got %s", c.mode)
	}
}

func TestSuspectedOutagePrewarmsWithoutRoutingAndCleansUp(t *testing.T) {
	c := testController()
	p := &recordingProvider{scales: make(chan int, 4)}
	c.cfg.Providers = []cloud.Provider{p}
	c.autoEvac = true
	c.seenLocal = true
	c.cloudSt = &siteagent.Status{Running: true}
	c.edgeState = &edge.StateView{Backends: []edge.BackendView{{BackendSpec: edge.BackendSpec{Name: "local", Group: "local", Enabled: true}, Healthy: false}}}

	c.decide(t.Context())
	select {
	case n := <-p.scales:
		if n != c.cfg.EvacMin {
			t.Fatalf("prewarm scaled to %d, want %d", n, c.cfg.EvacMin)
		}
	case <-time.After(time.Second):
		t.Fatal("suspected outage did not start prewarming")
	}
	if c.mode != ModeNormal || c.target != 0 || !c.localEnabled {
		t.Fatalf("prewarm changed routing: mode=%s target=%d local=%v", c.mode, c.target, c.localEnabled)
	}
	for _, b := range c.desiredBackends() {
		if b.Group == "cloud" && b.Units != 0 {
			t.Fatalf("prewarmed cloud received traffic: %+v", b)
		}
	}

	// The local site recovers before EVAC_AFTER: no DB promotion or evacuation.
	c.localSt = &siteagent.Status{Running: true}
	c.edgeState.Backends[0].Healthy = true
	deadline := time.Now().Add(time.Second)
	for len(c.prewarmDone) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	c.decide(t.Context())
	if c.mode != ModeNormal || c.scaleDownAt.IsZero() {
		t.Fatalf("recovery should schedule cleanup without evacuation: mode=%s downAt=%v", c.mode, c.scaleDownAt)
	}
	c.scaleDownAt = time.Now().Add(-time.Second)
	c.decide(t.Context())
	select {
	case n := <-p.scales:
		if n != 0 {
			t.Fatalf("cleanup scaled to %d, want 0", n)
		}
	case <-time.After(time.Second):
		t.Fatal("transient prewarm was not cleaned up")
	}
}

func TestEmergencyReadinessObservationSkipsDeadLocalAgent(t *testing.T) {
	var mu sync.Mutex
	localCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/local/status" {
			mu.Lock()
			localCalls++
			mu.Unlock()
			time.Sleep(500 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := testController()
	c.cfg.EdgeAdmin = srv.URL + "/edge"
	c.cfg.DBRouterAdmin = srv.URL + "/router"
	c.cfg.LocalAgent = srv.URL + "/local"
	c.cfg.CloudAgent = srv.URL + "/cloud"
	c.cfg.ProbeURL = srv.URL + "/probe"
	c.mode = ModeEvacuating
	c.localDownSince = time.Now().Add(-time.Minute)
	start := time.Now()
	c.observe(t.Context())
	if elapsed := time.Since(start); elapsed >= 300*time.Millisecond {
		t.Fatalf("emergency observation waited on local site: %v", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if localCalls != 0 || c.localSt != nil {
		t.Fatalf("unreachable local site was polled: calls=%d state=%+v", localCalls, c.localSt)
	}
}

func TestObservationRefreshesEdgeAfterLocalAgentTimeout(t *testing.T) {
	var mu sync.Mutex
	edgeReads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/edge/state":
			mu.Lock()
			edgeReads++
			read := edgeReads
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(edge.StateView{Backends: []edge.BackendView{{BackendSpec: edge.BackendSpec{Name: "local", Group: "local"}, Healthy: read == 1}}})
		case "/local/status":
			time.Sleep(100 * time.Millisecond)
		default:
			_, _ = w.Write([]byte("{}"))
		}
	}))
	defer srv.Close()
	c := testController()
	c.obs = httpx.NewClient(40*time.Millisecond, "")
	c.cfg.EdgeAdmin = srv.URL + "/edge"
	c.cfg.DBRouterAdmin = srv.URL + "/router"
	c.cfg.LocalAgent = srv.URL + "/local"
	c.cfg.CloudAgent = srv.URL + "/cloud"
	c.cfg.ProbeURL = srv.URL + "/probe"
	c.observe(t.Context())
	if edgeReads != 2 || c.localSt != nil || c.localAppHealthy() {
		t.Fatalf("stale edge state after timeout: edgeReads=%d local=%+v healthy=%t", edgeReads, c.localSt, c.localAppHealthy())
	}
}

type recordingProvider struct{ scales chan int }

func (p *recordingProvider) Name() string { return "recording" }
func (p *recordingProvider) Scale(_ context.Context, n int) error {
	p.scales <- n
	return nil
}
func (p *recordingProvider) Status(_ context.Context) cloud.Status {
	return cloud.Status{Provider: p.Name()}
}

type noopProvider struct{}

func (noopProvider) Name() string                          { return "noop" }
func (noopProvider) Scale(_ context.Context, _ int) error  { return nil }
func (noopProvider) Status(_ context.Context) cloud.Status { return cloud.Status{Provider: "noop"} }
