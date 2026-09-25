// Package dbrouter is a switchable TCP proxy in front of Postgres. Burst
// instances always connect here; during evacuation or failback the control
// plane pauses new connections, drops existing ones, retargets the router at
// the newly promoted primary and resumes — no application restart needed.
package dbrouter

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"spillway/internal/httpx"
)

// Status is returned by the admin API.
type Status struct {
	Target   string `json:"target"`
	Paused   bool   `json:"paused"`
	Active   int    `json:"active"`
	Total    int64  `json:"total"`
	Failed   int64  `json:"failed"`
	Switches int64  `json:"switches"`
}

// Config configures the router.
type Config struct {
	Listen      string
	AdminListen string
	Target      string
	Token       string
	HoldTimeout time.Duration
	DialTimeout time.Duration
}

// ConfigFromEnv reads the router configuration from the environment.
func ConfigFromEnv() Config {
	return Config{
		Listen:      httpx.Env("DBROUTER_LISTEN", ":6432"),
		AdminListen: httpx.Env("DBROUTER_ADMIN_LISTEN", ":7100"),
		Target:      httpx.Env("DBROUTER_TARGET", "localhost:5432"),
		Token:       httpx.Env("SPILLWAY_TOKEN", ""),
		HoldTimeout: httpx.EnvDur("DBROUTER_HOLD_TIMEOUT", 30*time.Second),
		DialTimeout: httpx.EnvDur("DBROUTER_DIAL_TIMEOUT", 3*time.Second),
	}
}

type pair struct{ client, upstream net.Conn }

func (p *pair) close() {
	p.client.Close()
	p.upstream.Close()
}

// Router is the TCP proxy.
type Router struct {
	cfg      Config
	mu       sync.Mutex
	target   string
	paused   bool
	changed  chan struct{}
	conns    map[*pair]struct{}
	total    atomic.Int64
	failed   atomic.Int64
	switches atomic.Int64
}

// New creates a router.
func New(cfg Config) *Router {
	return &Router{cfg: cfg, target: cfg.Target, changed: make(chan struct{}), conns: map[*pair]struct{}{}}
}

// Run serves TCP and the admin API until ctx is cancelled.
func (r *Router) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", r.cfg.Listen)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		if err := httpx.Serve(ctx, r.cfg.AdminListen, r.AdminHandler()); err != nil {
			log.Printf("dbrouter: admin: %v", err)
		}
	}()
	log.Printf("dbrouter: listening %s -> %s (admin %s)", r.cfg.Listen, r.target, r.cfg.AdminListen)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("dbrouter: accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go r.handle(c)
	}
}

func (r *Router) waitTarget() (string, bool) {
	deadline := time.Now().Add(r.cfg.HoldTimeout)
	for {
		r.mu.Lock()
		if !r.paused {
			t := r.target
			r.mu.Unlock()
			return t, true
		}
		ch := r.changed
		r.mu.Unlock()
		wait := time.Until(deadline)
		if wait <= 0 {
			return "", false
		}
		select {
		case <-ch:
		case <-time.After(wait):
		}
	}
}

func (r *Router) handle(c net.Conn) {
	r.total.Add(1)
	target, ok := r.waitTarget()
	if !ok {
		r.failed.Add(1)
		c.Close()
		return
	}
	up, err := net.DialTimeout("tcp", target, r.cfg.DialTimeout)
	if err != nil {
		r.failed.Add(1)
		log.Printf("dbrouter: dial %s: %v", target, err)
		c.Close()
		return
	}
	if tc, ok := up.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(10 * time.Second)
	}
	p := &pair{client: c, upstream: up}
	r.mu.Lock()
	// The target may have switched while we were dialing.
	if r.target != target || r.paused {
		r.mu.Unlock()
		p.close()
		return
	}
	r.conns[p] = struct{}{}
	r.mu.Unlock()

	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
	p.close()
	r.mu.Lock()
	delete(r.conns, p)
	r.mu.Unlock()
}

func (r *Router) killAllLocked() int {
	n := len(r.conns)
	for p := range r.conns {
		p.close()
		delete(r.conns, p)
	}
	return n
}

func (r *Router) notifyLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// Status returns the router state.
func (r *Router) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{
		Target: r.target, Paused: r.paused, Active: len(r.conns),
		Total: r.total.Load(), Failed: r.failed.Load(), Switches: r.switches.Load(),
	}
}

// AdminHandler exposes the router control API.
func (r *Router) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, r.Status())
	})
	mux.HandleFunc("PUT /target", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Addr string `json:"addr"`
			Kill *bool  `json:"kill"`
		}
		if err := httpx.ReadJSON(req, &body); err != nil || body.Addr == "" {
			httpx.Error(w, http.StatusBadRequest, "addr is required")
			return
		}
		r.mu.Lock()
		changed := r.target != body.Addr
		r.target = body.Addr
		killed := 0
		if body.Kill == nil || *body.Kill {
			killed = r.killAllLocked()
		}
		r.notifyLocked()
		r.mu.Unlock()
		if changed {
			r.switches.Add(1)
		}
		log.Printf("dbrouter: target -> %s (killed %d conns)", body.Addr, killed)
		httpx.WriteJSON(w, http.StatusOK, r.Status())
	})
	mux.HandleFunc("POST /pause", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Kill bool `json:"kill"`
		}
		_ = httpx.ReadJSON(req, &body)
		r.mu.Lock()
		r.paused = true
		killed := 0
		if body.Kill {
			killed = r.killAllLocked()
		}
		r.notifyLocked()
		r.mu.Unlock()
		log.Printf("dbrouter: paused (killed %d conns)", killed)
		httpx.WriteJSON(w, http.StatusOK, r.Status())
	})
	mux.HandleFunc("POST /kill", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		killed := r.killAllLocked()
		r.mu.Unlock()
		log.Printf("dbrouter: killed %d conns", killed)
		httpx.WriteJSON(w, http.StatusOK, r.Status())
	})
	mux.HandleFunc("POST /resume", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		r.paused = false
		r.notifyLocked()
		r.mu.Unlock()
		log.Printf("dbrouter: resumed")
		httpx.WriteJSON(w, http.StatusOK, r.Status())
	})
	return httpx.RequireToken(r.cfg.Token, mux)
}
