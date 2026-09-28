// Command guestbook is an ordinary user web app that Spillway deploys: a
// guestbook backed by Postgres. It knows nothing about Spillway beyond the app
// contract (PORT, DB_URL, SITE, GET /healthz; README "앱 계약"). Every response
// says which site served it so the audience can watch traffic move between the
// laptop and the cloud.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/proxy"
)

//go:embed page.html
var pageHTML string

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := New(ConfigFromEnv())
	if err == nil {
		err = a.Run(ctx)
	}
	if err != nil {
		log.Fatalf("guestbook: %v", err)
	}
}

// Config configures the app.
type Config struct {
	Listen      string
	Site        string
	Instance    string
	DBURL       string
	DBSocks5    string
	WorkMS      int
	MaxInflight int
}

// ConfigFromEnv reads the app configuration from the environment.
func ConfigFromEnv() Config {
	host, _ := os.Hostname()
	return Config{
		Listen:      ":" + env("PORT", "8080"),
		Site:        env("SITE", "local"),
		Instance:    env("INSTANCE", env("K_REVISION", host)),
		DBURL:       env("DB_URL", "postgres://spillway:spillway-secret@localhost:5432/spillway?sslmode=disable"),
		DBSocks5:    env("DB_SOCKS5", ""),
		WorkMS:      envInt("WORK_MS", 0),
		MaxInflight: envInt("MAX_INFLIGHT", 0),
	}
}

// Entry is one guestbook row.
type Entry struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Message   string    `json:"message"`
	Site      string    `json:"site"`
	CreatedAt time.Time `json:"created_at"`
}

// App is the guestbook server.
type App struct {
	cfg     Config
	pool    *pgxpool.Pool
	sem     chan struct{}
	ready   atomic.Bool
	tmpl    *template.Template
	started time.Time
}

// New connects the pool lazily and returns the app.
func New(cfg Config) (*App, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.DBURL)
	if err != nil {
		return nil, fmt.Errorf("DB_URL: %w", err)
	}
	pcfg.MaxConns = 16
	pcfg.MinConns = 0
	pcfg.MaxConnIdleTime = 30 * time.Second
	pcfg.HealthCheckPeriod = 5 * time.Second
	pcfg.ConnConfig.ConnectTimeout = 3 * time.Second
	if cfg.DBSocks5 != "" {
		d, err := proxy.SOCKS5("tcp", cfg.DBSocks5, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("DB_SOCKS5: %w", err)
		}
		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("socks5 dialer does not support contexts")
		}
		pcfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return cd.DialContext(ctx, network, addr)
		}
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, pool: pool, started: time.Now()}
	if cfg.MaxInflight > 0 {
		a.sem = make(chan struct{}, cfg.MaxInflight)
	}
	a.tmpl = template.Must(template.New("page").Parse(pageHTML))
	return a, nil
}

// Run migrates the schema in the background and serves HTTP.
func (a *App) Run(ctx context.Context) error {
	go a.migrateLoop(ctx)
	log.Printf("app[%s/%s]: listening on %s (work %dms, max inflight %d)", a.cfg.Site, a.cfg.Instance, a.cfg.Listen, a.cfg.WorkMS, a.cfg.MaxInflight)
	return serve(ctx, a.cfg.Listen, a.Handler())
}

const schema = `
CREATE TABLE IF NOT EXISTS entries (
	id         bigserial PRIMARY KEY,
	name       text        NOT NULL,
	message    text        NOT NULL,
	site       text        NOT NULL,
	client_id  text,
	seq        bigint,
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS entries_client_seq ON entries (client_id, seq) WHERE client_id IS NOT NULL;
`

func (a *App) migrateLoop(ctx context.Context) {
	for {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var exists bool
		err := a.pool.QueryRow(cctx, "SELECT to_regclass('public.entries') IS NOT NULL").Scan(&exists)
		if err == nil && !exists {
			_, err = a.pool.Exec(cctx, schema)
		}
		cancel()
		if err == nil {
			if !a.ready.Swap(true) {
				log.Printf("app: schema ready")
			}
			return
		}
		log.Printf("app: waiting for database: %v", err)
		select {
		case <-ctx.Done():
			return
		// A burst app may start before the DB router changes primary. Retry
		// promptly so an already-running instance becomes ready at cutover.
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Handler returns the HTTP routes.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /api/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.whoami())
	})
	mux.HandleFunc("GET /api/entries", a.listEntries)
	mux.HandleFunc("POST /api/entries", a.createEntry)
	mux.HandleFunc("GET /api/seqs", a.listSeqs)
	mux.HandleFunc("GET /api/work", func(w http.ResponseWriter, r *http.Request) {
		if !a.work(r.Context()) {
			return
		}
		writeJSON(w, http.StatusOK, a.whoami())
	})
	mux.HandleFunc("GET /{$}", a.page)
	return a.withServedBy(mux)
}

func (a *App) withServedBy(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", a.cfg.Site+"/"+a.cfg.Instance)
		h.ServeHTTP(w, r)
	})
}

func (a *App) whoami() map[string]any {
	return map[string]any{"site": a.cfg.Site, "instance": a.cfg.Instance, "uptime_s": int(time.Since(a.started).Seconds())}
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 700*time.Millisecond)
	defer cancel()
	if !a.ready.Load() {
		http.Error(w, "schema not ready", http.StatusServiceUnavailable)
		return
	}
	if err := a.pool.Ping(ctx); err != nil {
		http.Error(w, "db: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	io.WriteString(w, "ok")
}

// work simulates the CPU cost of rendering a page. With MAX_INFLIGHT set, the
// server has a fixed capacity and requests queue beyond it — the "small
// on-prem server" that Spillway bursts out of.
func (a *App) work(ctx context.Context) bool {
	if a.sem != nil {
		select {
		case a.sem <- struct{}{}:
			defer func() { <-a.sem }()
		case <-ctx.Done():
			return false
		}
	}
	if a.cfg.WorkMS <= 0 {
		return true
	}
	deadline := time.Now().Add(time.Duration(a.cfg.WorkMS) * time.Millisecond)
	sum := sha256.Sum256([]byte(a.cfg.Instance))
	for time.Now().Before(deadline) {
		for i := 0; i < 200; i++ {
			sum = sha256.Sum256(sum[:])
		}
	}
	return true
}

func (a *App) recent(ctx context.Context, limit int) ([]Entry, error) {
	rows, err := a.pool.Query(ctx, `SELECT id, name, message, site, created_at FROM entries
		WHERE client_id IS NULL OR client_id NOT LIKE 'probe-%' ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Entry])
}

func (a *App) page(w http.ResponseWriter, r *http.Request) {
	if !a.work(r.Context()) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	entries, err := a.recent(ctx, 30)
	var total int64
	if err == nil {
		err = a.pool.QueryRow(ctx, "SELECT count(*) FROM entries WHERE client_id IS NULL OR client_id NOT LIKE 'probe-%'").Scan(&total)
	}
	data := map[string]any{
		"Site": a.cfg.Site, "Instance": a.cfg.Instance, "Entries": entries,
		"Total": total, "Err": errString(err), "IsLocal": a.cfg.Site == "local",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := a.tmpl.Execute(w, data); err != nil {
		log.Printf("app: render: %v", err)
	}
}

func (a *App) listEntries(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	entries, err := a.recent(ctx, 30)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	var total int64
	_ = a.pool.QueryRow(ctx, "SELECT count(*) FROM entries WHERE client_id IS NULL OR client_id NOT LIKE 'probe-%'").Scan(&total)
	if entries == nil {
		entries = []Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"served_by": a.whoami(), "total": total, "entries": entries})
}

type createReq struct {
	Name     string `json:"name"`
	Message  string `json:"message"`
	ClientID string `json:"client_id"`
	Seq      *int64 `json:"seq"`
}

func (a *App) createEntry(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		r.ParseForm()
		req.Name, req.Message = r.FormValue("name"), r.FormValue("message")
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Message = strings.TrimSpace(req.Message)
	if req.Name == "" {
		req.Name = "익명"
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}
	if len([]rune(req.Name)) > 40 {
		req.Name = string([]rune(req.Name)[:40])
	}
	if len([]rune(req.Message)) > 280 {
		req.Message = string([]rune(req.Message)[:280])
	}
	var clientID any
	if req.ClientID != "" {
		clientID = req.ClientID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// Writes carrying (client_id, seq) are idempotent, so a connection error
	// during a failover can be retried safely.
	var id int64
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = a.pool.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO entries (name, message, site, client_id, seq) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (client_id, seq) WHERE client_id IS NOT NULL DO NOTHING
				RETURNING id)
			SELECT id FROM ins
			UNION ALL
			SELECT id FROM entries WHERE client_id = $4 AND seq = $5
			LIMIT 1`, req.Name, req.Message, a.cfg.Site, clientID, req.Seq).Scan(&id)
		if err == nil {
			break
		}
		if !(req.Seq != nil && clientID != nil) && !pgconn.SafeToRetry(err) {
			break
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(200*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "served_by": a.whoami()})
}

// listSeqs returns the sequence numbers stored for a probe client, used to
// count acknowledged writes that were lost in a failover.
func (a *App) listSeqs(w http.ResponseWriter, r *http.Request) {
	client := r.URL.Query().Get("client")
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	to, err := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if client == "" || err != nil {
		writeError(w, http.StatusBadRequest, "client and to are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := a.pool.Query(ctx, `SELECT seq FROM entries WHERE client_id = $1 AND seq BETWEEN $2 AND $3 ORDER BY seq`, client, from, to)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	seqs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if seqs == nil {
		seqs = []int64{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"seqs": seqs})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}

func serve(ctx context.Context, addr string, h http.Handler) error {
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return err
	}
	return json.Unmarshal(data, v)
}
