package control

import (
	"context"
	_ "embed"
	"io"
	"net/http"
	"time"

	"spillway/internal/httpx"
)

//go:embed dashboard.html
var dashboardHTML []byte

// Handler returns the dashboard and the control API.
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(dashboardHTML)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, c.Snapshot())
	})

	enqueue := func(kind string, arg bool) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			select {
			case c.cmds <- command{kind: kind, arg: arg}:
				httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "command": kind})
			default:
				httpx.Error(w, http.StatusServiceUnavailable, "command queue full")
			}
		}
	}
	mux.HandleFunc("POST /api/burst/on", enqueue("burst-on", true))
	mux.HandleFunc("POST /api/burst/off", enqueue("burst-off", false))
	mux.HandleFunc("POST /api/migrate", enqueue("migrate", true))
	mux.HandleFunc("POST /api/failback", enqueue("failback", true))
	mux.HandleFunc("POST /api/auto-evacuate", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		enqueue("auto-evac", body.Enabled)(w, r)
	})

	// Pass-through helpers so the dashboard only needs to talk to one origin.
	proxy := func(method, url string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body any
			if err := httpx.ReadJSON(r, &body); err != nil {
				httpx.Error(w, http.StatusBadRequest, err.Error())
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			var out any
			if err := c.ops.Do(ctx, method, url, body, &out); err != nil {
				httpx.Error(w, http.StatusBadGateway, err.Error())
				return
			}
			httpx.WriteJSON(w, http.StatusOK, out)
		}
	}
	mux.HandleFunc("POST /api/load", proxy("POST", c.cfg.ProbeURL+"/load"))
	mux.HandleFunc("POST /api/load/stop", proxy("POST", c.cfg.ProbeURL+"/load/stop"))
	mux.HandleFunc("POST /api/probe/reset", proxy("POST", c.cfg.ProbeURL+"/reset"))
	mux.HandleFunc("POST /api/sync-replication", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			On bool `json:"on"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		// Synchronous replication is configured on whichever site is primary.
		v := c.Snapshot()
		agent := c.cfg.LocalAgent
		if v.Cloud.Status != nil && v.Cloud.Status.Role == "primary" {
			agent = c.cfg.CloudAgent
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := c.ops.Do(ctx, "POST", agent+"/sync", body, nil); err != nil {
			httpx.Error(w, http.StatusBadGateway, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"sync": body.On})
	})
	return httpx.RequireToken(c.cfg.Token, mux)
}
