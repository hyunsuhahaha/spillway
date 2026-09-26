// Package control is the Spillway control plane. Once per second it observes
// the edge, both site agents, the DB router, the burst providers and the
// probe; decides which mode the system should be in (normal, burst,
// evacuated, failback); and executes the transitions step by step, recording
// how long each step took.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"spillway/internal/cloud"
	"spillway/internal/dbrouter"
	"spillway/internal/edge"
	"spillway/internal/httpx"
	"spillway/internal/probe"
	"spillway/internal/siteagent"
)

// Mode is the system-wide operating mode.
type Mode string

const (
	ModeNormal        Mode = "NORMAL"
	ModeBurst         Mode = "BURST"
	ModeEvacuating    Mode = "EVACUATING"
	ModeEvacuated     Mode = "EVACUATED"
	ModeFailbackSync  Mode = "FAILBACK_SYNC"
	ModeSwitchingBack Mode = "SWITCHING_BACK"
)

// Config configures the control plane.
type Config struct {
	Listen    string
	Token     string
	PublicURL string
	EventLog  string // JSON-lines audit log of events and operations ("" = memory only)

	EdgeAdmin     string
	DBRouterAdmin string
	ProbeURL      string
	LocalAgent    string
	CloudAgent    string

	LocalAppURL string
	LocalUnits  int

	RouterLocalPG   string // how the DB router reaches the local primary
	RouterCloudPG   string // how the DB router reaches the cloud database
	CloudPGForLocal string // how the local site reaches the cloud DB (failback clone)
	LocalPGForCloud string // how the cloud site reaches the local DB (re-protect clone)

	P95High      float64
	P95Low       float64
	MinRPS       float64
	Sustain      time.Duration
	StepCooldown time.Duration
	ScaleInAfter time.Duration
	EvacAfter    time.Duration
	AppDownAfter time.Duration
	BurstMin     int
	BurstMax     int
	EvacMin      int
	WarmMin      int
	AutoEvacuate bool

	CostPerInstanceSecondKRW float64
	StandbyCostPerHourKRW    float64

	Providers []cloud.Provider
}

// ConfigFromEnv reads the control-plane configuration from the environment.
func ConfigFromEnv() (Config, error) {
	providers, err := cloud.FromEnv()
	if err != nil {
		return Config{}, err
	}
	return Config{
		Listen:        httpx.Env("CONTROL_LISTEN", ":8090"),
		Token:         httpx.Env("SPILLWAY_TOKEN", ""),
		PublicURL:     httpx.Env("PUBLIC_URL", "http://localhost:8080"),
		EventLog:      httpx.Env("EVENT_LOG", ""),
		EdgeAdmin:     httpx.Env("EDGE_ADMIN_URL", "http://edge:8081"),
		DBRouterAdmin: httpx.Env("DBROUTER_ADMIN_URL", "http://dbrouter:7100"),
		ProbeURL:      httpx.Env("PROBE_URL", "http://probe:7200"),
		LocalAgent:    httpx.Env("LOCAL_AGENT_URL", "http://local-db:7000"),
		CloudAgent:    httpx.Env("CLOUD_AGENT_URL", "http://cloud-db:7000"),

		LocalAppURL: httpx.Env("LOCAL_APP_URL", "http://local-app:8080"),
		LocalUnits:  httpx.EnvInt("LOCAL_UNITS", 2),

		RouterLocalPG:   httpx.Env("ROUTER_LOCAL_PG", "local-db:5432"),
		RouterCloudPG:   httpx.Env("ROUTER_CLOUD_PG", "cloud-db:5432"),
		CloudPGForLocal: httpx.Env("CLOUD_PG_FOR_LOCAL", "cloud-db:5432"),
		LocalPGForCloud: httpx.Env("LOCAL_PG_FOR_CLOUD", "local-db:5432"),

		P95High:      httpx.EnvFloat("BURST_P95_HIGH_MS", 250),
		P95Low:       httpx.EnvFloat("BURST_P95_LOW_MS", 120),
		MinRPS:       httpx.EnvFloat("BURST_MIN_RPS", 2),
		Sustain:      httpx.EnvDur("BURST_SUSTAIN", 3*time.Second),
		StepCooldown: httpx.EnvDur("BURST_STEP_COOLDOWN", 8*time.Second),
		ScaleInAfter: httpx.EnvDur("BURST_SCALE_IN_AFTER", 15*time.Second),
		EvacAfter:    httpx.EnvDur("EVAC_AFTER", 3*time.Second),
		AppDownAfter: httpx.EnvDur("APP_DOWN_AFTER", 3*time.Second),
		BurstMin:     httpx.EnvInt("BURST_MIN", 2),
		BurstMax:     httpx.EnvInt("BURST_MAX", 6),
		EvacMin:      httpx.EnvInt("EVAC_MIN", 2),
		WarmMin:      httpx.EnvInt("CLOUD_WARM_MIN", 0),
		AutoEvacuate: httpx.EnvBool("AUTO_EVACUATE", true),

		CostPerInstanceSecondKRW: httpx.EnvFloat("COST_PER_INSTANCE_SECOND_KRW", 0.035),
		StandbyCostPerHourKRW:    httpx.EnvFloat("STANDBY_COST_PER_HOUR_KRW", 45),

		Providers: providers,
	}, nil
}

// Event is one line of the audit log.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // info | action | warn | error
	Msg  string    `json:"msg"`
}

// Step is one timed step of an operation.
type Step struct {
	Name   string `json:"name"`
	MS     int64  `json:"ms"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Operation is a recorded evacuation, migration or failback.
type Operation struct {
	Kind     string    `json:"kind"`
	Reason   string    `json:"reason"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	TotalMS  int64     `json:"total_ms"`
	Running  bool      `json:"running"`
	OK       bool      `json:"ok"`
	Steps    []Step    `json:"steps"`
}

// Point is one sample of the dashboard time series.
type Point struct {
	T          int64   `json:"t"`
	P95        float64 `json:"p95"`
	UpP95      float64 `json:"up_p95"`
	RPS        float64 `json:"rps"`
	LocalShare float64 `json:"local_share"`
	Cloud      int     `json:"cloud"`
	Held       int64   `json:"held"`
}

// SiteView is what the dashboard knows about one site.
type SiteView struct {
	Name       string            `json:"name"`
	Reachable  bool              `json:"reachable"`
	Err        string            `json:"err,omitempty"`
	Status     *siteagent.Status `json:"status,omitempty"`
	AppHealthy bool              `json:"app_healthy"`
	AppEnabled bool              `json:"app_enabled"`
}

// Protection summarises whether a site failure right now would be survivable.
type Protection struct {
	State    string `json:"state"` // ok | degraded | none
	Detail   string `json:"detail"`
	LagBytes int64  `json:"lag_bytes"`
}

// View is the full state served to the dashboard.
type View struct {
	Now             time.Time        `json:"now"`
	Mode            Mode             `json:"mode"`
	ModeSince       time.Time        `json:"mode_since"`
	Reason          string           `json:"reason"`
	AutoEvacuate    bool             `json:"auto_evacuate"`
	ManualBurst     bool             `json:"manual_burst"`
	BurstTarget     int              `json:"burst_target"`
	LocalCapRPS     float64          `json:"local_capacity_rps"`
	LocalReturned   bool             `json:"local_returned"`
	Local           SiteView         `json:"local"`
	Cloud           SiteView         `json:"cloud"`
	Edge            *edge.StateView  `json:"edge,omitempty"`
	EdgeErr         string           `json:"edge_err,omitempty"`
	Router          *dbrouter.Status `json:"router,omitempty"`
	RouterErr       string           `json:"router_err,omitempty"`
	Providers       []cloud.Status   `json:"providers"`
	Probe           *probe.Status    `json:"probe,omitempty"`
	ProbeErr        string           `json:"probe_err,omitempty"`
	Operation       *Operation       `json:"operation,omitempty"`
	Operations      []Operation      `json:"operations"`
	Events          []Event          `json:"events"`
	History         []Point          `json:"history"`
	InstanceSeconds float64          `json:"instance_seconds"`
	CostKRW         float64          `json:"cost_krw"`
	Protection      Protection       `json:"protection"`
	Settings        map[string]any   `json:"settings"`
}

type command struct {
	kind string
	arg  bool
}

// Controller owns the decision loop. Fields below "loop-owned" are only
// touched by the loop goroutine; the view is shared under mu.
type Controller struct {
	cfg  Config
	obs  *httpx.Client // short timeouts for observation
	ops  *httpx.Client // longer timeouts for operations
	cmds chan command

	mu   sync.Mutex
	view View

	// loop-owned
	mode           Mode
	modeSince      time.Time
	reason         string
	autoEvac       bool
	manualBurst    bool
	target         int
	localCap       float64 // measured local throughput while saturated (rps), per burst episode
	lastLocalCap   float64 // last measured value, kept for the dashboard
	localEnabled   bool
	highSince      time.Time
	lowSince       time.Time
	localDownSince time.Time
	appDownSince   time.Time
	lastScale      time.Time
	scaleDownAt    time.Time
	lastEdgeSpec   string
	lastTick       time.Time
	warnedNoCloud  bool
	quietUntil     time.Time // no load-based decisions until then (after a switch)
	seenLocal      bool      // local site was healthy at least once (no evacuation during boot)

	edgeState  *edge.StateView
	localSt    *siteagent.Status
	cloudSt    *siteagent.Status
	routerSt   *dbrouter.Status
	probeSt    *probe.Status
	provSt     []cloud.Status
	instSecs   float64
	history    []Point
	events     []Event
	operations []Operation
	curOp      *Operation
}

// New creates a controller.
func New(cfg Config) *Controller {
	return &Controller{
		cfg:          cfg,
		obs:          httpx.NewClient(1500*time.Millisecond, cfg.Token),
		ops:          httpx.NewClient(45*time.Second, cfg.Token),
		cmds:         make(chan command, 16),
		mode:         ModeNormal,
		modeSince:    time.Now(),
		autoEvac:     cfg.AutoEvacuate,
		localEnabled: true,
	}
}

// Run starts the decision loop and the HTTP API.
func (c *Controller) Run(ctx context.Context) error {
	c.loadAudit()
	c.event("info", fmt.Sprintf("컨트롤 플레인 시작 (버스트 대상: %s)", c.providerNames()))
	go c.loop(ctx)
	log.Printf("control: api/dashboard on %s", c.cfg.Listen)
	return httpx.Serve(ctx, c.cfg.Listen, c.Handler())
}

func (c *Controller) providerNames() string {
	var names []string
	for _, p := range c.cfg.Providers {
		names = append(names, p.Name())
	}
	if len(names) == 0 {
		return "없음"
	}
	return strings.Join(names, ", ")
}

// reconcile restores the mode from the databases' actual roles when the
// control plane starts, so a restart during an evacuation does not route
// traffic back to a stale local primary.
func (c *Controller) reconcile(ctx context.Context) {
	if c.cloudSt == nil || c.cloudSt.Role != "primary" || c.cloudSt.Fenced {
		return
	}
	c.localEnabled = false
	c.target = max(c.cfg.EvacMin, 1)
	c.setMode(ModeEvacuated, "재시작 시 복원: 클라우드 DB가 주 DB")
	if err := c.routerCall(ctx, "PUT", "/target", map[string]any{"addr": c.cfg.RouterCloudPG, "kill": true}); err != nil {
		c.event("error", "DB 라우터 복원 실패: "+err.Error())
	}
	if err := c.scaleProviders(ctx, c.target); err != nil {
		c.event("error", "클라우드 인스턴스 복원 실패: "+err.Error())
	}
	c.event("warn", "컨트롤 플레인 재시작: 클라우드 DB가 주 DB이므로 대피 상태를 복원했습니다")
}

func (c *Controller) loop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	c.observe(ctx)
	c.reconcile(ctx)
	if c.cfg.WarmMin > 0 && c.mode == ModeNormal {
		if err := c.scaleProviders(ctx, c.cfg.WarmMin); err != nil {
			c.event("error", "대기 인스턴스 예열 실패: "+err.Error())
		} else {
			c.event("info", fmt.Sprintf("대피 대비 클라우드 인스턴스 %d개 예열 (트래픽 0%%)", c.cfg.WarmMin))
		}
	}
	c.syncEdge(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return
		case cm := <-c.cmds:
			c.handleCommand(ctx, cm)
		case <-t.C:
			c.observe(ctx)
			c.decide(ctx)
			c.syncEdge(ctx, false)
		}
		c.publish()
	}
}

// ---------------------------------------------------------------- observation

func (c *Controller) observe(ctx context.Context) {
	var wg sync.WaitGroup
	var edgeSt edge.StateView
	var routerSt dbrouter.Status
	var localSt, cloudSt siteagent.Status
	var probeSt probe.Status
	var edgeErr, routerErr, localErr, cloudErr, probeErr error
	provSt := make([]cloud.Status, len(c.cfg.Providers))

	get := func(url string, out any, errp *error) {
		defer wg.Done()
		*errp = c.obs.Do(ctx, "GET", url, nil, out)
	}
	wg.Add(5)
	go get(c.cfg.EdgeAdmin+"/state", &edgeSt, &edgeErr)
	go get(c.cfg.DBRouterAdmin+"/status", &routerSt, &routerErr)
	go get(c.cfg.LocalAgent+"/status", &localSt, &localErr)
	go get(c.cfg.CloudAgent+"/status", &cloudSt, &cloudErr)
	go get(c.cfg.ProbeURL+"/status", &probeSt, &probeErr)
	for i, p := range c.cfg.Providers {
		wg.Add(1)
		go func(i int, p cloud.Provider) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			provSt[i] = p.Status(pctx)
		}(i, p)
	}
	wg.Wait()

	now := time.Now()
	c.edgeState = nil
	if edgeErr == nil {
		c.edgeState = &edgeSt
	}
	c.routerSt = nil
	if routerErr == nil {
		c.routerSt = &routerSt
	}
	c.localSt = nil
	if localErr == nil {
		c.localSt = &localSt
	}
	c.cloudSt = nil
	if cloudErr == nil {
		c.cloudSt = &cloudSt
	}
	c.probeSt = nil
	if probeErr == nil {
		c.probeSt = &probeSt
	}
	c.provSt = provSt
	if c.localSt != nil && c.localSt.Running && c.localAppHealthy() {
		c.seenLocal = true
	}

	// Accumulate billed instance time for the cost estimate.
	if !c.lastTick.IsZero() {
		dt := now.Sub(c.lastTick).Seconds()
		if dt > 5 {
			dt = 5
		}
		c.instSecs += float64(c.cloudInstances()) * dt
	}
	c.lastTick = now

	if c.edgeState != nil {
		st := c.edgeState.Stats
		local := c.edgeState.Groups["local"].Count
		cloudN := c.edgeState.Groups["cloud"].Count
		share := 1.0
		if local+cloudN > 0 {
			share = float64(local) / float64(local+cloudN)
		}
		c.history = append(c.history, Point{T: now.UnixMilli(), P95: st.P95, UpP95: c.edgeState.Upstream.P95, RPS: st.RPS, LocalShare: share, Cloud: c.cloudInstances(), Held: c.edgeState.Held})
		if len(c.history) > 300 {
			c.history = c.history[len(c.history)-300:]
		}
	}

	c.mu.Lock()
	c.view.EdgeErr, c.view.RouterErr, c.view.ProbeErr = errStr(edgeErr), errStr(routerErr), errStr(probeErr)
	c.view.Local.Err, c.view.Cloud.Err = errStr(localErr), errStr(cloudErr)
	c.mu.Unlock()
}

func (c *Controller) cloudInstances() int {
	n := 0
	for _, s := range c.provSt {
		n += s.Ready
	}
	return n
}

func (c *Controller) localBackend() *edge.BackendView {
	if c.edgeState == nil {
		return nil
	}
	for i := range c.edgeState.Backends {
		if c.edgeState.Backends[i].Name == "local" {
			return &c.edgeState.Backends[i]
		}
	}
	return nil
}

func (c *Controller) localAppHealthy() bool {
	b := c.localBackend()
	return b != nil && b.Healthy
}

func (c *Controller) cloudHealthy() int {
	if c.edgeState == nil {
		return 0
	}
	n := 0
	for _, b := range c.edgeState.Backends {
		if b.Group == "cloud" && b.Healthy && b.Enabled && b.Units > 0 {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- decisions

func (c *Controller) setMode(m Mode, reason string) {
	if c.mode != m {
		log.Printf("control: mode %s -> %s (%s)", c.mode, m, reason)
	}
	c.mode, c.modeSince, c.reason = m, time.Now(), reason
	c.highSince, c.lowSince = time.Time{}, time.Time{}
}

func (c *Controller) decide(ctx context.Context) {
	now := time.Now()
	if !c.scaleDownAt.IsZero() && now.After(c.scaleDownAt) {
		c.scaleDownAt = time.Time{}
		if c.target == 0 {
			if err := c.scaleProviders(ctx, c.cfg.WarmMin); err != nil {
				c.event("error", "클라우드 축소 실패: "+err.Error())
			} else if c.cfg.WarmMin > 0 {
				c.event("info", fmt.Sprintf("클라우드 인스턴스를 대기용 %d개로 축소했습니다 (트래픽 0%%, 대피 대비 예열)", c.cfg.WarmMin))
			} else {
				c.event("info", "클라우드 인스턴스를 0개로 축소했습니다 (비용 0)")
			}
		}
	}
	if c.edgeState == nil {
		return // cannot decide without the edge's view
	}
	// Load decisions use backend service time, not user-seen latency, so the
	// seconds a request spends held during a switch never look like overload.
	st := c.edgeState.Upstream
	st.RPS = c.edgeState.Stats.RPS
	if now.Before(c.quietUntil) {
		c.highSince = time.Time{}
	}

	high := st.P95 > c.cfg.P95High && st.RPS >= c.cfg.MinRPS && !now.Before(c.quietUntil)
	// While the local site is saturated, its completed-request rate is its
	// capacity. Remember the highest value seen during this burst episode.
	if lg, ok := c.edgeState.UpstreamGroups["local"]; ok && lg.P95 > c.cfg.P95High && lg.RPS > c.localCap {
		c.localCap = lg.RPS
		c.lastLocalCap = lg.RPS
	}
	low := st.P95 < c.cfg.P95Low || st.RPS < c.cfg.MinRPS
	c.highSince = since(c.highSince, high, now)
	c.lowSince = since(c.lowSince, low, now)

	localAgentUp := c.localSt != nil && c.localSt.Running
	localDown := !c.localAppHealthy() && c.localSt == nil
	c.localDownSince = since(c.localDownSince, localDown, now)
	appDown := !c.localAppHealthy() && localAgentUp
	c.appDownSince = since(c.appDownSince, appDown, now)

	switch c.mode {
	case ModeNormal, ModeBurst:
		if c.autoEvac && c.seenLocal && held(c.localDownSince, now, c.cfg.EvacAfter) {
			if c.cloudSt == nil || !c.cloudSt.Running {
				if !c.warnedNoCloud {
					c.event("error", "로컬 사이트가 응답하지 않지만 클라우드 DB 에이전트에도 연결할 수 없어 대피할 수 없습니다")
					c.warnedNoCloud = true
				}
				return
			}
			c.warnedNoCloud = false
			c.evacuate(ctx, false, fmt.Sprintf("로컬 사이트 응답 없음 %.0f초 (앱·DB 에이전트 모두 연결 불가)", dur(c.localDownSince, now).Seconds()))
			return
		}
		appDownLong := held(c.appDownSince, now, c.cfg.AppDownAfter)
		if c.mode == ModeNormal {
			switch {
			case appDownLong:
				c.enterBurst(ctx, max(c.cfg.BurstMin, c.cfg.EvacMin), "로컬 앱 응답 없음 → DB는 로컬 유지, 트래픽은 클라우드 앱으로")
			case held(c.highSince, now, c.cfg.Sustain):
				c.enterBurst(ctx, c.cfg.BurstMin, fmt.Sprintf("p95 %.0fms > %.0fms (%.0fs 지속, %.1f rps)", st.P95, c.cfg.P95High, c.cfg.Sustain.Seconds(), st.RPS))
			}
			return
		}
		// BURST
		if appDownLong && c.target < c.cfg.EvacMin {
			c.scaleTo(ctx, c.cfg.EvacMin, "로컬 앱 응답 없음")
		}
		if held(c.highSince, now, c.cfg.Sustain) && now.Sub(c.lastScale) >= c.cfg.StepCooldown && c.target < c.cfg.BurstMax {
			c.scaleTo(ctx, c.target+1, fmt.Sprintf("버스트 중에도 p95 %.0fms로 높음", st.P95))
		}
		// Scale in only when the whole load would fit on the local site again.
		quiet := held(c.lowSince, now, c.cfg.ScaleInAfter) && (c.localCap == 0 || st.RPS < c.localCap*0.7)
		if !c.manualBurst && !appDown && quiet && now.Sub(c.lastScale) >= c.cfg.ScaleInAfter {
			reason := fmt.Sprintf("부하 감소 (%.1f rps, 백엔드 p95 %.0fms)", st.RPS, st.P95)
			if c.localCap > 0 {
				reason = fmt.Sprintf("부하 감소 (%.1f rps < 실측 로컬 한계 %.0f rps의 70%%, 백엔드 p95 %.0fms)", st.RPS, c.localCap, st.P95)
			}
			c.exitBurst(ctx, reason)
		}

	case ModeEvacuated:
		returned := c.localSt != nil && c.localSt.Running
		if returned && !c.view.LocalReturned {
			c.event("action", "로컬 사이트 복귀 감지 — 페일백을 실행할 수 있습니다")
		}
		c.mu.Lock()
		c.view.LocalReturned = returned
		c.mu.Unlock()
		if returned && c.localSt.Role == "primary" && !c.localSt.Fenced {
			// The old primary came back still believing it is primary.
			if err := c.ops.Do(ctx, "POST", c.cfg.LocalAgent+"/fence", map[string]bool{"on": true}, nil); err != nil {
				c.event("error", "로컬 DB 격리 실패: "+err.Error())
			} else {
				c.event("warn", "스플릿 브레인 방지: 복귀한 로컬 DB를 읽기 전용으로 격리했습니다")
			}
		}
		// Cloud is the only site: it may still scale with load.
		if held(c.highSince, now, c.cfg.Sustain) && now.Sub(c.lastScale) >= c.cfg.StepCooldown && c.target < c.cfg.BurstMax {
			c.scaleTo(ctx, c.target+1, fmt.Sprintf("대피 중 p95 %.0fms", st.P95))
		}
		if held(c.lowSince, now, c.cfg.ScaleInAfter) && c.target > c.cfg.EvacMin && now.Sub(c.lastScale) >= c.cfg.ScaleInAfter {
			c.scaleTo(ctx, c.cfg.EvacMin, "대피 중 부하 감소")
		}
	}
}

func since(t time.Time, cond bool, now time.Time) time.Time {
	if !cond {
		return time.Time{}
	}
	if t.IsZero() {
		return now
	}
	return t
}

// held reports whether a condition that started at t (zero = not holding)
// has held for at least d.
func held(t, now time.Time, d time.Duration) bool {
	return !t.IsZero() && now.Sub(t) >= d
}

func dur(t, now time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return now.Sub(t)
}

func (c *Controller) enterBurst(ctx context.Context, n int, reason string) {
	if len(c.cfg.Providers) == 0 {
		c.event("warn", "버스트 조건이지만 설정된 클라우드 제공자가 없습니다")
		return
	}
	c.setMode(ModeBurst, reason)
	c.event("action", "버스트 시작: "+reason)
	c.scaleTo(ctx, n, "버스트 시작")
}

func (c *Controller) exitBurst(ctx context.Context, reason string) {
	c.setMode(ModeNormal, reason)
	c.target = 0
	c.localCap = 0
	c.lastScale = time.Now()
	c.syncEdge(ctx, true)
	c.scaleDownAt = time.Now().Add(5 * time.Second) // let in-flight requests drain first
	c.event("action", "버스트 종료: "+reason+" — 클라우드 트래픽 0%, 5초 뒤 인스턴스 축소")
}

func (c *Controller) scaleTo(ctx context.Context, n int, reason string) {
	if n > c.cfg.BurstMax {
		n = c.cfg.BurstMax
	}
	prev := c.target
	c.target = n
	c.lastScale = time.Now()
	c.scaleDownAt = time.Time{}
	if err := c.scaleProviders(ctx, n); err != nil {
		c.event("error", "클라우드 확장 실패: "+err.Error())
		return
	}
	if n != prev {
		c.event("action", fmt.Sprintf("클라우드 인스턴스 %d → %d (%s)", prev, n, reason))
	}
}

func (c *Controller) scaleProviders(ctx context.Context, n int) error {
	parts := cloud.Split(n, len(c.cfg.Providers))
	var wg sync.WaitGroup
	errs := make([]string, 0)
	var emu sync.Mutex
	for i, p := range c.cfg.Providers {
		wg.Add(1)
		go func(p cloud.Provider, k int) {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := p.Scale(sctx, k); err != nil {
				emu.Lock()
				errs = append(errs, p.Name()+": "+err.Error())
				emu.Unlock()
			}
		}(p, parts[i])
	}
	wg.Wait()
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// desiredBackends is the routing table the edge should have right now.
func (c *Controller) desiredBackends() []edge.BackendSpec {
	units := c.cfg.LocalUnits
	specs := []edge.BackendSpec{{Name: "local", URL: c.cfg.LocalAppURL, Group: "local", Units: units, Enabled: c.localEnabled}}
	for _, st := range c.provSt {
		for _, ep := range st.Endpoints {
			// An idle Cloud Run endpoint must not stay in the edge's health
			// checker: each probe is a request that can wake a zero-min service.
			// Keep warm-min endpoints so their readiness can still be observed.
			if c.target == 0 && c.cfg.WarmMin == 0 && st.Kind == "cloudrun" && st.Desired == 0 {
				continue
			}
			u := ep.Units
			if c.target == 0 {
				u = 0
			}
			specs = append(specs, edge.BackendSpec{Name: ep.Name, URL: ep.URL, Group: "cloud", Units: u, Enabled: true})
		}
	}
	return specs
}

func (c *Controller) syncEdge(ctx context.Context, force bool) error {
	specs := c.desiredBackends()
	data, _ := json.Marshal(specs)
	if !force && string(data) == c.lastEdgeSpec {
		return nil
	}
	if err := c.ops.Do(ctx, "PUT", c.cfg.EdgeAdmin+"/backends", map[string]any{"backends": specs}, nil); err != nil {
		return err
	}
	c.lastEdgeSpec = string(data)
	return nil
}

// ---------------------------------------------------------------- operations

type opRunner struct {
	c      *Controller
	op     *Operation
	failed bool
}

func (c *Controller) beginOp(kind, reason string) *opRunner {
	op := &Operation{Kind: kind, Reason: reason, Started: time.Now(), Running: true, Steps: []Step{}}
	c.curOp = op
	c.publish()
	return &opRunner{c: c, op: op}
}

// step runs fn unless an earlier step failed, and records its duration.
func (r *opRunner) step(name string, fn func() error) bool {
	if r.failed {
		return false
	}
	start := time.Now()
	err := fn()
	s := Step{Name: name, MS: time.Since(start).Milliseconds(), OK: err == nil}
	if err != nil {
		s.Detail = err.Error()
		r.failed = true
		r.c.event("error", fmt.Sprintf("%s 실패: %v", name, err))
	}
	r.op.Steps = append(r.op.Steps, s)
	r.c.publish()
	return err == nil
}

func (r *opRunner) finish() {
	r.op.Running = false
	r.op.OK = !r.failed
	r.op.Finished = time.Now()
	r.op.TotalMS = r.op.Finished.Sub(r.op.Started).Milliseconds()
	r.c.appendAudit(auditRecord{Type: "operation", Operation: r.op})
	r.c.operations = append(r.c.operations, *r.op)
	if len(r.c.operations) > 20 {
		r.c.operations = r.c.operations[len(r.c.operations)-20:]
	}
	r.c.publish()
}

func (c *Controller) edgeCall(ctx context.Context, path string, body any) error {
	return c.ops.Do(ctx, "POST", c.cfg.EdgeAdmin+path, body, nil)
}

func (c *Controller) routerCall(ctx context.Context, method, path string, body any) error {
	return c.ops.Do(ctx, method, c.cfg.DBRouterAdmin+path, body, nil)
}

func (c *Controller) agentStatus(ctx context.Context, url string) (*siteagent.Status, error) {
	var st siteagent.Status
	if err := c.obs.Do(ctx, "GET", url+"/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// waitFor polls cond every interval until it returns true or timeout passes,
// refreshing the observed state (and the dashboard) while it waits.
func (c *Controller) waitFor(ctx context.Context, timeout, interval time.Duration, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := cond()
		if ok {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("%s: 시간 초과 (%v)", what, lastErr)
			}
			return fmt.Errorf("%s: 시간 초과", what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// evacuate moves the whole service to the cloud site. planned=true is a
// controlled migration with zero data loss (writes are frozen until the
// standby has replayed everything); planned=false is the emergency path taken
// when the local site is unreachable.
func (c *Controller) evacuate(ctx context.Context, planned bool, reason string) {
	kind, title := "evacuate", "긴급 대피"
	if planned {
		kind, title = "migrate", "계획된 이사"
	}
	prevMode := c.mode
	c.setMode(ModeEvacuating, reason)
	c.event("action", fmt.Sprintf("%s 시작: %s", title, reason))
	r := c.beginOp(kind, reason)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	edgePaused, routerPaused, promoted := false, false, false
	defer func() {
		if edgePaused {
			c.edgeCall(context.Background(), "/resume", nil)
		}
		if routerPaused {
			c.routerCall(context.Background(), "POST", "/resume", nil)
		}
	}()

	r.step("엣지: 새 요청을 잠시 붙잡음 (에러 대신 대기)", func() error {
		edgePaused = true
		return c.edgeCall(ctx, "/pause", map[string]string{"reason": kind})
	})
	evacN := max(c.cfg.EvacMin, c.target)
	c.target = evacN
	c.lastScale = time.Now()
	c.scaleDownAt = time.Time{}
	scaleDone := make(chan error, 1)
	go func() { scaleDone <- c.scaleProviders(ctx, evacN) }()

	r.step("엣지: 로컬 사이트를 라우팅에서 제외 (격리)", func() error {
		c.localEnabled = false
		return c.syncEdge(ctx, true)
	})

	var lossBytes int64 = -1
	if planned {
		r.step("DB 라우터: 새 연결 대기, 진행 중 쿼리 마무리", func() error {
			routerPaused = true
			if err := c.routerCall(ctx, "POST", "/pause", map[string]bool{"kill": false}); err != nil {
				return err
			}
			time.Sleep(800 * time.Millisecond)
			return c.routerCall(ctx, "POST", "/kill", nil)
		})
		r.step("로컬 DB 쓰기 차단 (fence)", func() error {
			return c.ops.Do(ctx, "POST", c.cfg.LocalAgent+"/fence", map[string]bool{"on": true}, nil)
		})
		r.step("클라우드 복제본이 마지막 쓰기까지 따라잡기 (RPO 0)", func() error {
			return c.waitFor(ctx, 30*time.Second, 200*time.Millisecond, "복제 동기화", func() (bool, error) {
				l, err := c.agentStatus(ctx, c.cfg.LocalAgent)
				if err != nil {
					return false, err
				}
				cl, err := c.agentStatus(ctx, c.cfg.CloudAgent)
				if err != nil {
					return false, err
				}
				lossBytes = l.LSNBytes - cl.LSNBytes
				return cl.LSNBytes >= l.LSNBytes, nil
			})
		})
	} else {
		r.step("DB 라우터: 끊긴 로컬 DB로 가는 연결 정리", func() error {
			routerPaused = true
			return c.routerCall(ctx, "POST", "/pause", map[string]bool{"kill": true})
		})
	}
	r.step("클라우드 DB 승격 (대기 복제본 → 주 DB)", func() error {
		if err := c.ops.Do(ctx, "POST", c.cfg.CloudAgent+"/promote", nil, nil); err != nil {
			return err
		}
		promoted = true
		return nil
	})
	r.step("DB 라우터: 클라우드 DB로 전환", func() error {
		if err := c.routerCall(ctx, "PUT", "/target", map[string]any{"addr": c.cfg.RouterCloudPG, "kill": true}); err != nil {
			return err
		}
		routerPaused = false
		return c.routerCall(ctx, "POST", "/resume", nil)
	})
	r.step(fmt.Sprintf("클라우드 앱 %d개 준비 + 헬스체크 통과", evacN), func() error {
		if err := <-scaleDone; err != nil {
			return err
		}
		return c.waitFor(ctx, 90*time.Second, 300*time.Millisecond, "클라우드 앱 준비", func() (bool, error) {
			c.observe(ctx)
			c.syncEdge(ctx, false)
			c.publish()
			return c.cloudHealthy() > 0, nil
		})
	})
	r.step("엣지: 붙잡아 둔 요청을 클라우드로 흘려보냄", func() error {
		edgePaused = false
		return c.edgeCall(ctx, "/resume", nil)
	})
	r.finish()

	c.quietUntil = time.Now().Add(8 * time.Second)
	switch {
	case !r.failed:
		c.setMode(ModeEvacuated, reason)
		msg := fmt.Sprintf("%s 완료: 전환 %.1f초", title, float64(r.op.TotalMS)/1000)
		if planned {
			msg += " · 복제 지연 0 확인 (데이터 손실 없음)"
		}
		c.event("action", msg)
	case planned && !promoted:
		// Roll the planned migration back: local stays primary.
		c.ops.Do(context.Background(), "POST", c.cfg.LocalAgent+"/fence", map[string]bool{"on": false}, nil)
		c.routerCall(context.Background(), "PUT", "/target", map[string]any{"addr": c.cfg.RouterLocalPG, "kill": true})
		c.localEnabled = true
		c.syncEdge(context.Background(), true)
		c.setMode(prevMode, "이사 실패로 원상복구")
		c.event("warn", "계획된 이사를 취소하고 로컬을 주 사이트로 유지했습니다")
	default:
		c.setMode(ModeEvacuated, reason+" (일부 단계 실패)")
		c.event("error", "대피가 완전히 끝나지 않았습니다. 작업 기록을 확인하세요")
	}
	_ = lossBytes
}

// failback brings the service home: the local database is rebuilt as a
// standby of the cloud, catches up, and then a planned switchover makes it
// primary again. Finally the cloud database is re-cloned from local so the
// system is protected again.
func (c *Controller) failback(ctx context.Context) {
	if c.mode != ModeEvacuated {
		c.event("warn", "페일백은 대피 완료 상태에서만 실행할 수 있습니다")
		return
	}
	if c.localSt == nil {
		c.event("warn", "로컬 DB 에이전트에 연결할 수 없어 페일백할 수 없습니다")
		return
	}
	reason := "운영자 승인"
	c.setMode(ModeFailbackSync, reason)
	c.event("action", "페일백 시작: 로컬 DB를 클라우드의 복제본으로 재구성합니다")
	r := c.beginOp("failback", reason)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	cloudHost, cloudPort := splitHostPort(c.cfg.CloudPGForLocal)
	localHost, localPort := splitHostPort(c.cfg.LocalPGForCloud)

	edgePaused, routerPaused := false, false
	defer func() {
		if edgePaused {
			c.edgeCall(context.Background(), "/resume", nil)
		}
		if routerPaused {
			c.routerCall(context.Background(), "POST", "/resume", nil)
		}
	}()

	r.step("로컬 DB 재구성 (클라우드 DB에서 베이스 백업)", func() error {
		return c.ops.Do(ctx, "POST", c.cfg.LocalAgent+"/rebuild", map[string]any{"primary_host": cloudHost, "primary_port": cloudPort}, nil)
	})
	r.step("로컬 복제본 스트리밍 + 지연 1MB 미만", func() error {
		time.Sleep(time.Second)
		return c.waitFor(ctx, 15*time.Minute, time.Second, "로컬 복제 동기화", func() (bool, error) {
			c.observe(ctx)
			c.publish()
			if c.localSt == nil || c.cloudSt == nil {
				return false, errors.New("에이전트 연결 불가")
			}
			if c.localSt.State != "running" || c.localSt.Role != "standby" || !c.localSt.Streaming {
				return false, fmt.Errorf("로컬 상태 %s/%s", c.localSt.State, c.localSt.Role)
			}
			for _, rep := range c.cloudSt.Replicas {
				if rep.Name == c.localSt.Site && rep.LagBytes >= 0 && rep.LagBytes < 1<<20 {
					return true, nil
				}
			}
			return false, nil
		})
	})
	if !r.failed {
		c.setMode(ModeSwitchingBack, reason)
	}
	r.step("엣지 + DB 라우터: 새 요청 대기, 진행 중 쿼리 마무리", func() error {
		edgePaused = true
		if err := c.edgeCall(ctx, "/pause", map[string]string{"reason": "failback"}); err != nil {
			return err
		}
		routerPaused = true
		if err := c.routerCall(ctx, "POST", "/pause", map[string]bool{"kill": false}); err != nil {
			return err
		}
		time.Sleep(800 * time.Millisecond)
		return c.routerCall(ctx, "POST", "/kill", nil)
	})
	r.step("로컬 복제본이 마지막 쓰기까지 따라잡기 (RPO 0)", func() error {
		return c.waitFor(ctx, 30*time.Second, 200*time.Millisecond, "복제 동기화", func() (bool, error) {
			l, err := c.agentStatus(ctx, c.cfg.LocalAgent)
			if err != nil {
				return false, err
			}
			cl, err := c.agentStatus(ctx, c.cfg.CloudAgent)
			if err != nil {
				return false, err
			}
			return l.LSNBytes >= cl.LSNBytes, nil
		})
	})
	r.step("로컬 DB 승격 (주 DB 복귀)", func() error {
		return c.ops.Do(ctx, "POST", c.cfg.LocalAgent+"/promote", nil, nil)
	})
	r.step("클라우드 DB 쓰기 차단 (fence)", func() error {
		return c.ops.Do(ctx, "POST", c.cfg.CloudAgent+"/fence", map[string]bool{"on": true}, nil)
	})
	r.step("DB 라우터: 로컬 DB로 전환", func() error {
		if err := c.routerCall(ctx, "PUT", "/target", map[string]any{"addr": c.cfg.RouterLocalPG, "kill": true}); err != nil {
			return err
		}
		routerPaused = false
		return c.routerCall(ctx, "POST", "/resume", nil)
	})
	r.step("엣지: 로컬 사이트 재편입, 클라우드 트래픽 0%", func() error {
		c.localEnabled = true
		c.target = 0
		c.localCap = 0
		if err := c.syncEdge(ctx, true); err != nil {
			return err
		}
		return c.waitFor(ctx, 20*time.Second, 300*time.Millisecond, "로컬 앱 헬스체크", func() (bool, error) {
			c.observe(ctx)
			return c.localAppHealthy(), nil
		})
	})
	r.step("엣지: 붙잡아 둔 요청을 로컬로 흘려보냄", func() error {
		edgePaused = false
		return c.edgeCall(ctx, "/resume", nil)
	})
	r.step("클라우드 DB를 로컬의 복제본으로 재구성 (보호 재개)", func() error {
		return c.ops.Do(ctx, "POST", c.cfg.CloudAgent+"/rebuild", map[string]any{"primary_host": localHost, "primary_port": localPort}, nil)
	})
	r.finish()

	if r.failed {
		if c.mode == ModeFailbackSync {
			c.setMode(ModeEvacuated, "페일백 실패")
		} else {
			c.setMode(ModeEvacuated, "페일백 전환 실패")
		}
		c.event("error", "페일백이 완료되지 않았습니다. 작업 기록을 확인하세요")
		return
	}
	c.quietUntil = time.Now().Add(8 * time.Second)
	c.setMode(ModeNormal, "페일백 완료")
	c.lastScale = time.Now()
	c.scaleDownAt = time.Now().Add(5 * time.Second)
	c.mu.Lock()
	c.view.LocalReturned = false
	c.mu.Unlock()
	c.event("action", fmt.Sprintf("페일백 완료: 로컬이 다시 주 사이트입니다 (%.1f초). 클라우드 DB는 복제본으로 재구성 중", float64(r.op.TotalMS)/1000))
}

func splitHostPort(addr string) (string, int) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 5432
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		n = 5432
	}
	return h, n
}

func (c *Controller) handleCommand(ctx context.Context, cm command) {
	switch cm.kind {
	case "burst-on":
		c.manualBurst = true
		if c.mode == ModeNormal {
			c.enterBurst(ctx, c.cfg.BurstMin, "운영자가 수동으로 버스트 시작")
		}
	case "burst-off":
		c.manualBurst = false
		if c.mode == ModeBurst {
			c.exitBurst(ctx, "운영자가 수동으로 버스트 종료")
		}
	case "migrate":
		if c.mode != ModeNormal && c.mode != ModeBurst {
			c.event("warn", "이사는 평상/버스트 상태에서만 실행할 수 있습니다")
			return
		}
		if c.localSt == nil || c.cloudSt == nil {
			c.event("warn", "두 사이트의 DB 에이전트가 모두 연결되어 있어야 이사할 수 있습니다")
			return
		}
		c.evacuate(ctx, true, "운영자가 클라우드로 계획된 이사 실행")
	case "failback":
		c.failback(ctx)
	case "auto-evac":
		c.autoEvac = cm.arg
		state := "끔"
		if cm.arg {
			state = "켬"
		}
		c.event("info", "자동 대피 "+state)
	}
}

// ---------------------------------------------------------------- view

func (c *Controller) event(kind, msg string) {
	e := Event{At: time.Now(), Kind: kind, Msg: msg}
	log.Printf("control: [%s] %s", kind, msg)
	c.appendAudit(auditRecord{Type: "event", Event: &e})
	c.events = append(c.events, e)
	if len(c.events) > 300 {
		c.events = c.events[len(c.events)-300:]
	}
}

func (c *Controller) protection() Protection {
	switch c.mode {
	case ModeEvacuated, ModeEvacuating:
		return Protection{State: "degraded", Detail: "클라우드 단독 운영 중 — 로컬 복귀 후 페일백하면 보호가 재개됩니다", LagBytes: -1}
	case ModeFailbackSync, ModeSwitchingBack:
		return Protection{State: "degraded", Detail: "페일백 진행 중", LagBytes: -1}
	}
	if c.localSt == nil {
		return Protection{State: "none", Detail: "로컬 DB 상태를 알 수 없음", LagBytes: -1}
	}
	if c.localSt.Role != "primary" {
		return Protection{State: "degraded", Detail: "로컬 DB가 주 DB가 아님", LagBytes: -1}
	}
	for _, rep := range c.localSt.Replicas {
		if rep.State == "streaming" {
			mode := "비동기"
			if rep.Sync == "sync" {
				mode = "동기"
			}
			return Protection{State: "ok", Detail: fmt.Sprintf("클라우드 복제본 스트리밍 중 (%s)", mode), LagBytes: rep.LagBytes}
		}
	}
	if c.cloudSt != nil && c.cloudSt.State == "rebuilding" {
		return Protection{State: "degraded", Detail: "클라우드 복제본 재구성 중", LagBytes: -1}
	}
	return Protection{State: "degraded", Detail: "클라우드 복제본 연결 끊김 — 지금 로컬이 죽으면 대피할 수 없음", LagBytes: -1}
}

func (c *Controller) publish() {
	v := View{
		Now: time.Now(), Mode: c.mode, ModeSince: c.modeSince, Reason: c.reason,
		AutoEvacuate: c.autoEvac, ManualBurst: c.manualBurst, BurstTarget: c.target, LocalCapRPS: c.lastLocalCap,
		Edge: c.edgeState, Router: c.routerSt, Providers: c.provSt, Probe: c.probeSt,
		Operation: c.curOp, Operations: append([]Operation{}, c.operations...),
		Events: append([]Event{}, c.events...), History: append([]Point{}, c.history...),
		InstanceSeconds: c.instSecs, CostKRW: c.instSecs * c.cfg.CostPerInstanceSecondKRW,
		Protection: c.protection(),
		Settings: map[string]any{
			"p95_high_ms": c.cfg.P95High, "p95_low_ms": c.cfg.P95Low, "sustain_s": c.cfg.Sustain.Seconds(),
			"burst_min": c.cfg.BurstMin, "burst_max": c.cfg.BurstMax, "evac_min": c.cfg.EvacMin, "warm_min": c.cfg.WarmMin,
			"evac_after_s": c.cfg.EvacAfter.Seconds(), "local_units": c.cfg.LocalUnits,
			"standby_cost_per_hour_krw": c.cfg.StandbyCostPerHourKRW, "providers": c.providerNames(),
			"cost_per_instance_second_krw": c.cfg.CostPerInstanceSecondKRW,
			"public_url":                   c.cfg.PublicURL,
		},
	}
	if v.Providers == nil {
		v.Providers = []cloud.Status{}
	}
	v.Local = SiteView{Name: "local", Reachable: c.localSt != nil, Status: c.localSt, AppEnabled: c.localEnabled, AppHealthy: c.localAppHealthy()}
	v.Cloud = SiteView{Name: "cloud", Reachable: c.cloudSt != nil, Status: c.cloudSt, AppEnabled: true, AppHealthy: c.cloudHealthy() > 0}
	c.mu.Lock()
	v.LocalReturned = c.view.LocalReturned
	v.EdgeErr, v.RouterErr, v.ProbeErr = c.view.EdgeErr, c.view.RouterErr, c.view.ProbeErr
	v.Local.Err, v.Cloud.Err = c.view.Local.Err, c.view.Cloud.Err
	c.view = v
	c.mu.Unlock()
}

// Snapshot returns the current view.
func (c *Controller) Snapshot() View {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.view
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
