package control

import (
	"context"
	"testing"
	"time"

	"spillway/internal/cloud"
	"spillway/internal/edge"
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

type noopProvider struct{}

func (noopProvider) Name() string                          { return "noop" }
func (noopProvider) Scale(_ context.Context, _ int) error  { return nil }
func (noopProvider) Status(_ context.Context) cloud.Status { return cloud.Status{Provider: "noop"} }
