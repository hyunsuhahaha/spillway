// Package probe measures what users actually experience. It writes a numbered
// entry through the edge several times per second, then reads the sequence
// numbers back to count acknowledged writes that were lost, and measures the
// longest gap between successful writes (client-observed RTO). It also hosts
// a load generator used to trigger bursts during the demo.
package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"spillway/internal/httpx"
	"spillway/internal/metrics"
)

// Outage is a window during which no write succeeded.
type Outage struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Seconds  float64   `json:"seconds"`
	Failures int       `json:"failures"`
}

// LoadStatus describes the load generator.
type LoadStatus struct {
	Running  bool             `json:"running"`
	RPS      int              `json:"rps"`
	Until    time.Time        `json:"until"`
	Sent     int64            `json:"sent"`
	Dropped  int64            `json:"dropped"`
	Failed   int64            `json:"failed"`
	Stats    metrics.Stats    `json:"stats"`
	ByTarget map[string]int64 `json:"by_backend"`
}

// Status is returned by GET /status.
type Status struct {
	ClientID    string     `json:"client_id"`
	Running     bool       `json:"running"`
	Seq         int64      `json:"seq"`
	Acked       int64      `json:"acked"`
	Failed      int64      `json:"failed"`
	Verified    int64      `json:"verified"`
	Lost        int64      `json:"lost"`
	LostSeqs    []int64    `json:"lost_seqs"`
	Pending     int        `json:"pending"`
	CurrentGap  float64    `json:"current_gap_s"`
	LastWriteMS float64    `json:"last_write_ms"`
	LastBackend string     `json:"last_backend"`
	Outages     []Outage   `json:"outages"`
	LastOutage  *Outage    `json:"last_outage,omitempty"`
	Load        LoadStatus `json:"load"`
}

// Config configures the probe.
type Config struct {
	Listen      string
	Target      string
	Token       string
	Interval    time.Duration
	OutageMin   time.Duration
	VerifyEvery time.Duration
	VerifyGrace time.Duration
	LoadPath    string
	LoadWorkers int
}

// ConfigFromEnv reads the probe configuration from the environment.
func ConfigFromEnv() Config {
	return Config{
		Listen:      httpx.Env("PROBE_LISTEN", ":7200"),
		Target:      httpx.Env("PROBE_TARGET", "http://edge:8080"),
		Token:       httpx.Env("SPILLWAY_TOKEN", ""),
		Interval:    httpx.EnvDur("PROBE_INTERVAL", 200*time.Millisecond),
		OutageMin:   httpx.EnvDur("PROBE_OUTAGE_MIN", time.Second),
		VerifyEvery: httpx.EnvDur("PROBE_VERIFY_EVERY", 3*time.Second),
		VerifyGrace: httpx.EnvDur("PROBE_VERIFY_GRACE", 2*time.Second),
		LoadPath:    httpx.Env("PROBE_LOAD_PATH", "/api/work"),
		LoadWorkers: httpx.EnvInt("PROBE_LOAD_WORKERS", 256),
	}
}

type ack struct {
	seq    int64
	at     time.Time
	misses int
}

// Probe is the measurement service.
type Probe struct {
	cfg    Config
	client *http.Client

	mu          sync.Mutex
	clientID    string
	seq         int64
	acked       int64
	failed      int64
	verified    int64
	lostSeqs    []int64
	pending     map[int64]*ack
	lastOK      time.Time
	gapFailures int
	outages     []Outage
	lastWriteMS float64
	lastBackend string
	epoch       int64

	load loadGen
}

type loadGen struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	rps      int
	until    time.Time
	sent     atomic.Int64
	dropped  atomic.Int64
	failed   atomic.Int64
	win      *metrics.Window
	byTarget sync.Map // backend name -> *atomic.Int64
}

// New creates a probe.
func New(cfg Config) *Probe {
	p := &Probe{
		cfg:    cfg,
		client: &http.Client{Timeout: 40 * time.Second},
	}
	p.load.win = metrics.NewWindow(10 * time.Second)
	p.reset()
	return p
}

func newClientID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "probe-" + hex.EncodeToString(b)
}

func (p *Probe) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clientID = newClientID()
	p.seq, p.acked, p.failed, p.verified = 0, 0, 0, 0
	p.lostSeqs = nil
	p.pending = map[int64]*ack{}
	p.lastOK = time.Time{}
	p.gapFailures = 0
	p.outages = nil
	p.epoch++
}

// Run starts the writer, the verifier and the API.
func (p *Probe) Run(ctx context.Context) error {
	go p.writer(ctx)
	go p.verifier(ctx)
	log.Printf("probe: target %s, api %s", p.cfg.Target, p.cfg.Listen)
	return httpx.Serve(ctx, p.cfg.Listen, p.Handler())
}

func (p *Probe) writer(ctx context.Context) {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.writeOnce(ctx)
	}
}

func (p *Probe) writeOnce(ctx context.Context) {
	p.mu.Lock()
	p.seq++
	seq, client, epoch := p.seq, p.clientID, p.epoch
	p.mu.Unlock()

	body, _ := json.Marshal(map[string]any{"name": "probe", "message": fmt.Sprintf("probe write #%d", seq), "client_id": client, "seq": seq})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Target+"/api/entries", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("%s-%d", client, seq))
	start := time.Now()
	resp, err := p.client.Do(req)
	ok := false
	backend := ""
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		ok = resp.StatusCode == http.StatusCreated
		backend = resp.Header.Get("X-Spillway-Backend")
	}
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	if epoch != p.epoch {
		return
	}
	p.lastWriteMS = float64(now.Sub(start).Microseconds()) / 1000
	if !ok {
		p.failed++
		p.gapFailures++
		return
	}
	p.acked++
	p.lastBackend = backend
	p.pending[seq] = &ack{seq: seq, at: now}
	// The gap is measured from the last success to this success; a slow
	// (held) request counts because the user was waiting the whole time.
	if !p.lastOK.IsZero() {
		if gap := now.Sub(p.lastOK); gap >= p.cfg.OutageMin+p.cfg.Interval {
			o := Outage{Start: p.lastOK, End: now, Seconds: round2(gap.Seconds()), Failures: p.gapFailures}
			p.outages = append(p.outages, o)
			log.Printf("probe: write gap of %.2fs (%d failed attempts)", o.Seconds, o.Failures)
		}
	}
	p.lastOK = now
	p.gapFailures = 0
}

// verifier reads back acknowledged sequence numbers. An acked write that is
// missing on two consecutive checks is counted as lost.
func (p *Probe) verifier(ctx context.Context) {
	t := time.NewTicker(p.cfg.VerifyEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.mu.Lock()
		client, epoch := p.clientID, p.epoch
		var seqs []int64
		cut := time.Now().Add(-p.cfg.VerifyGrace)
		for s, a := range p.pending {
			if a.at.Before(cut) {
				seqs = append(seqs, s)
			}
		}
		p.mu.Unlock()
		if len(seqs) == 0 {
			continue
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		found, err := p.fetchSeqs(ctx, client, seqs[0], seqs[len(seqs)-1])
		if err != nil {
			continue
		}
		p.mu.Lock()
		if epoch == p.epoch {
			for _, s := range seqs {
				a := p.pending[s]
				if a == nil {
					continue
				}
				if found[s] {
					p.verified++
					delete(p.pending, s)
					continue
				}
				a.misses++
				if a.misses >= 2 {
					p.lostSeqs = append(p.lostSeqs, s)
					delete(p.pending, s)
					log.Printf("probe: acknowledged write #%d is missing (lost)", s)
				}
			}
		}
		p.mu.Unlock()
	}
}

func (p *Probe) fetchSeqs(ctx context.Context, client string, from, to int64) (map[int64]bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/api/seqs?client=%s&from=%d&to=%d", p.cfg.Target, client, from, to)
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("seqs: %s", resp.Status)
	}
	var body struct {
		Seqs []int64 `json:"seqs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	m := make(map[int64]bool, len(body.Seqs))
	for _, s := range body.Seqs {
		m[s] = true
	}
	return m, nil
}

// ---------------------------------------------------------------- load generator

// StartLoad runs an open-loop load of rps requests per second for d.
func (p *Probe) StartLoad(rps int, d time.Duration) {
	p.StopLoad()
	if rps <= 0 || d <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	lg := &p.load
	lg.mu.Lock()
	lg.cancel, lg.rps, lg.until = cancel, rps, time.Now().Add(d)
	lg.mu.Unlock()
	lg.sent.Store(0)
	lg.dropped.Store(0)
	lg.failed.Store(0)
	lg.byTarget.Range(func(k, _ any) bool { lg.byTarget.Delete(k); return true })

	jobs := make(chan struct{}, p.cfg.LoadWorkers)
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: p.cfg.LoadWorkers}}
	for i := 0; i < p.cfg.LoadWorkers; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-jobs:
				}
				start := time.Now()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.Target+p.cfg.LoadPath, nil)
				resp, err := client.Do(req)
				ok := false
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					ok = resp.StatusCode < 500
					name := resp.Header.Get("X-Spillway-Backend")
					v, _ := lg.byTarget.LoadOrStore(name, new(atomic.Int64))
					v.(*atomic.Int64).Add(1)
				} else if ctx.Err() != nil {
					return
				}
				if !ok {
					lg.failed.Add(1)
				}
				lg.win.Add(metrics.Sample{At: time.Now(), Dur: time.Since(start), OK: ok})
			}
		}()
	}
	go func() {
		interval := time.Second / time.Duration(rps)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Printf("probe: load finished (sent %d, dropped %d)", lg.sent.Load(), lg.dropped.Load())
				return
			case <-t.C:
			}
			select {
			case jobs <- struct{}{}:
				lg.sent.Add(1)
			default:
				lg.dropped.Add(1)
			}
		}
	}()
	log.Printf("probe: load %d rps for %s", rps, d)
}

// StopLoad stops the load generator.
func (p *Probe) StopLoad() {
	p.load.mu.Lock()
	if p.load.cancel != nil {
		p.load.cancel()
		p.load.cancel = nil
	}
	p.load.mu.Unlock()
}

// Status returns the probe status.
func (p *Probe) Status() Status {
	p.mu.Lock()
	st := Status{
		ClientID: p.clientID, Running: true, Seq: p.seq, Acked: p.acked, Failed: p.failed,
		Verified: p.verified, Lost: int64(len(p.lostSeqs)), LostSeqs: append([]int64{}, p.lostSeqs...),
		Pending: len(p.pending), LastWriteMS: p.lastWriteMS, LastBackend: p.lastBackend,
		Outages: append([]Outage{}, p.outages...),
	}
	if !p.lastOK.IsZero() {
		st.CurrentGap = round2(time.Since(p.lastOK).Seconds())
	}
	if n := len(p.outages); n > 0 {
		o := p.outages[n-1]
		st.LastOutage = &o
	}
	p.mu.Unlock()

	lg := &p.load
	lg.mu.Lock()
	running := lg.cancel != nil && time.Now().Before(lg.until)
	st.Load = LoadStatus{Running: running, RPS: lg.rps, Until: lg.until}
	lg.mu.Unlock()
	st.Load.Sent, st.Load.Dropped, st.Load.Failed = lg.sent.Load(), lg.dropped.Load(), lg.failed.Load()
	st.Load.Stats = lg.win.Stats("")
	st.Load.ByTarget = map[string]int64{}
	lg.byTarget.Range(func(k, v any) bool { st.Load.ByTarget[k.(string)] = v.(*atomic.Int64).Load(); return true })
	return st
}

// Handler returns the probe API.
func (p *Probe) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, p.Status())
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, _ *http.Request) {
		p.reset()
		httpx.WriteJSON(w, http.StatusOK, p.Status())
	})
	mux.HandleFunc("POST /load", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RPS     int `json:"rps"`
			Seconds int `json:"seconds"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.RPS <= 0 {
			httpx.Error(w, http.StatusBadRequest, "rps must be > 0")
			return
		}
		if body.RPS > 5000 {
			body.RPS = 5000
		}
		if body.Seconds <= 0 {
			body.Seconds = 60
		}
		p.StartLoad(body.RPS, time.Duration(body.Seconds)*time.Second)
		httpx.WriteJSON(w, http.StatusOK, p.Status().Load)
	})
	mux.HandleFunc("POST /load/stop", func(w http.ResponseWriter, _ *http.Request) {
		p.StopLoad()
		httpx.WriteJSON(w, http.StatusOK, p.Status().Load)
	})
	return httpx.RequireToken(p.cfg.Token, mux)
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }
