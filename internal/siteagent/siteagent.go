// Package siteagent supervises the Postgres server of one site (local or
// cloud). It bootstraps the node as a primary or as a streaming standby,
// reports replication state, and executes the role changes the control plane
// asks for: promote, fence (read-only), and rebuild-as-standby.
package siteagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"spillway/internal/httpx"
)

// Config configures the agent.
type Config struct {
	Site         string
	InitRole     string // "primary" or "standby"
	PGData       string
	PGPort       int
	SocketDir    string
	PrimaryHost  string
	PrimaryPort  int
	ReplUser     string
	ReplPassword string
	AppUser      string
	AppPassword  string
	AppDB        string
	Listen       string
	Token        string
}

// ConfigFromEnv reads the agent configuration from the environment.
func ConfigFromEnv() Config {
	return Config{
		Site:         httpx.Env("SITE", "local"),
		InitRole:     httpx.Env("ROLE_INIT", "primary"),
		PGData:       httpx.Env("PGDATA", "/var/lib/postgresql/data/pgdata"),
		PGPort:       httpx.EnvInt("PG_PORT", 5432),
		SocketDir:    httpx.Env("PG_SOCKET_DIR", "/var/run/postgresql"),
		PrimaryHost:  httpx.Env("PRIMARY_HOST", ""),
		PrimaryPort:  httpx.EnvInt("PRIMARY_PORT", 5432),
		ReplUser:     httpx.Env("REPL_USER", "replicator"),
		ReplPassword: httpx.Env("REPL_PASSWORD", "replicator-secret"),
		AppUser:      httpx.Env("APP_DB_USER", "spillway"),
		AppPassword:  httpx.Env("APP_DB_PASSWORD", "spillway-secret"),
		AppDB:        httpx.Env("APP_DB_NAME", "spillway"),
		Listen:       httpx.Env("AGENT_LISTEN", ":7000"),
		Token:        httpx.Env("SPILLWAY_TOKEN", ""),
	}
}

// Replica describes one connected standby (seen from a primary).
type Replica struct {
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	State    string `json:"state"`
	Sync     string `json:"sync"`
	LagBytes int64  `json:"lag_bytes"`
}

// Status is returned by GET /status.
type Status struct {
	Site         string    `json:"site"`
	State        string    `json:"state"` // starting|running|rebuilding|stopped|error
	Running      bool      `json:"running"`
	Role         string    `json:"role"` // primary|standby|unknown
	Fenced       bool      `json:"fenced"`
	LSN          string    `json:"lsn,omitempty"`
	LSNBytes     int64     `json:"lsn_bytes"`
	ReceiveLSN   string    `json:"receive_lsn,omitempty"`
	Timeline     int       `json:"timeline"`
	Upstream     string    `json:"upstream,omitempty"`
	Streaming    bool      `json:"streaming"`
	SyncMode     bool      `json:"sync_mode"`
	Replicas     []Replica `json:"replicas"`
	LastErr      string    `json:"last_err,omitempty"`
	ChangedAt    time.Time `json:"changed_at"`
	RebuildCount int       `json:"rebuild_count"`
}

// Agent supervises one Postgres server.
type Agent struct {
	cfg Config

	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{}
	state    string
	lastErr  string
	upstream string
	changed  time.Time
	rebuilds int
	maint    bool // true while we intentionally stop/rebuild postgres

	opMu sync.Mutex // serialises role operations
}

// New creates an agent.
func New(cfg Config) *Agent { return &Agent{cfg: cfg, state: "starting", changed: time.Now()} }

func (a *Agent) setState(s, errMsg string) {
	a.mu.Lock()
	a.state, a.lastErr, a.changed = s, errMsg, time.Now()
	a.mu.Unlock()
}

// Run bootstraps postgres and serves the agent API until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	go func() {
		if err := httpx.Serve(ctx, a.cfg.Listen, a.Handler()); err != nil {
			log.Printf("agent: api: %v", err)
		}
	}()
	log.Printf("agent[%s]: api on %s, pgdata %s, init role %s", a.cfg.Site, a.cfg.Listen, a.cfg.PGData, a.cfg.InitRole)

	fresh := !exists(filepath.Join(a.cfg.PGData, "PG_VERSION"))
	if fresh {
		var err error
		if a.cfg.InitRole == "standby" {
			err = a.cloneFrom(ctx, a.cfg.PrimaryHost, a.cfg.PrimaryPort)
		} else {
			err = a.initPrimary(ctx)
		}
		if err != nil {
			return err
		}
	} else {
		a.mu.Lock()
		a.upstream = a.readUpstream()
		a.mu.Unlock()
	}
	if err := a.startPG(ctx); err != nil {
		return err
	}
	if fresh && a.cfg.InitRole != "standby" {
		if err := a.bootstrapRoles(ctx); err != nil {
			return fmt.Errorf("bootstrap roles: %w", err)
		}
	}
	go a.supervise(ctx)
	<-ctx.Done()
	a.stopPG()
	return nil
}

// ---------------------------------------------------------------- postgres lifecycle

func (a *Agent) initPrimary(ctx context.Context) error {
	a.setState("initializing", "")
	if err := os.MkdirAll(a.cfg.PGData, 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "initdb", "-D", a.cfg.PGData, "-U", "postgres",
		"--auth-local=trust", "--auth-host=scram-sha-256", "--encoding=UTF8", "--locale=C")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("initdb: %w", err)
	}
	conf := fmt.Sprintf(`
# ---- spillway ----
listen_addresses = '*'
port = %d
unix_socket_directories = '%s'
max_connections = 200
wal_level = replica
max_wal_senders = 10
wal_keep_size = '1GB'
hot_standby = on
hot_standby_feedback = on
wal_log_hints = on
wal_sender_timeout = '10s'
wal_receiver_timeout = '10s'
wal_retrieve_retry_interval = '1s'
password_encryption = 'scram-sha-256'
`, a.cfg.PGPort, a.cfg.SocketDir)
	if err := appendFile(filepath.Join(a.cfg.PGData, "postgresql.conf"), conf); err != nil {
		return err
	}
	hba := "local all all trust\n" +
		"host replication " + a.cfg.ReplUser + " all scram-sha-256\n" +
		"host all all all scram-sha-256\n"
	return os.WriteFile(filepath.Join(a.cfg.PGData, "pg_hba.conf"), []byte(hba), 0o600)
}

func (a *Agent) bootstrapRoles(ctx context.Context) error {
	conn, err := a.connect(ctx, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	stmts := []string{
		fmt.Sprintf("CREATE ROLE %s WITH REPLICATION LOGIN PASSWORD %s", ident(a.cfg.ReplUser), literal(a.cfg.ReplPassword)),
		fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s", ident(a.cfg.AppUser), literal(a.cfg.AppPassword)),
		fmt.Sprintf("CREATE DATABASE %s OWNER %s", ident(a.cfg.AppDB), ident(a.cfg.AppUser)),
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil && !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}
	log.Printf("agent[%s]: primary bootstrapped (db %s)", a.cfg.Site, a.cfg.AppDB)
	return nil
}

// cloneFrom replaces the data directory with a base backup of host:port and
// configures the node as a streaming standby of it. Retries until success.
func (a *Agent) cloneFrom(ctx context.Context, host string, port int) error {
	if host == "" {
		return errors.New("standby needs PRIMARY_HOST")
	}
	a.setState("rebuilding", "")
	for attempt := 1; ; attempt++ {
		if err := os.RemoveAll(a.cfg.PGData); err != nil {
			return err
		}
		if err := os.MkdirAll(a.cfg.PGData, 0o700); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "pg_basebackup", "-h", host, "-p", fmt.Sprint(port),
			"-U", a.cfg.ReplUser, "-D", a.cfg.PGData, "-X", "stream", "-c", "fast", "--no-password")
		cmd.Env = append(os.Environ(), "PGPASSWORD="+a.cfg.ReplPassword, "PGCONNECT_TIMEOUT=5")
		out, err := cmd.CombinedOutput()
		if err == nil {
			break
		}
		msg := strings.TrimSpace(string(out))
		a.setState("rebuilding", fmt.Sprintf("base backup from %s:%d failed (attempt %d): %s", host, port, attempt, msg))
		log.Printf("agent[%s]: pg_basebackup attempt %d: %v: %s", a.cfg.Site, attempt, err, msg)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err := os.Chmod(a.cfg.PGData, 0o700); err != nil {
		return err
	}
	for _, f := range []string{"standby.signal", "recovery.signal", "postmaster.pid"} {
		os.Remove(filepath.Join(a.cfg.PGData, f))
	}
	conninfo := fmt.Sprintf("host=%s port=%d user=%s password=%s application_name=%s connect_timeout=5",
		host, port, a.cfg.ReplUser, a.cfg.ReplPassword, a.cfg.Site)
	if err := rewriteAutoConf(a.cfg.PGData, map[string]string{
		"primary_conninfo":              literal(conninfo),
		"recovery_target_timeline":      "'latest'",
		"default_transaction_read_only": "",
		"synchronous_standby_names":     "",
	}); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(a.cfg.PGData, "standby.signal"), nil, 0o600); err != nil {
		return err
	}
	a.mu.Lock()
	a.upstream = fmt.Sprintf("%s:%d", host, port)
	a.rebuilds++
	a.mu.Unlock()
	log.Printf("agent[%s]: cloned from %s:%d, configured as standby", a.cfg.Site, host, port)
	return nil
}

func (a *Agent) startPG(ctx context.Context) error {
	cmd := exec.Command("postgres", "-D", a.cfg.PGData)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start postgres: %w", err)
	}
	exited := make(chan struct{})
	a.mu.Lock()
	a.cmd, a.exited = cmd, exited
	a.mu.Unlock()
	go func() { cmd.Wait(); close(exited) }()

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			a.setState("error", "postgres exited during startup")
			return errors.New("postgres exited during startup")
		default:
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := a.connect(cctx, "postgres")
		cancel()
		if err == nil {
			conn.Close(context.Background())
			a.setState("running", "")
			log.Printf("agent[%s]: postgres ready", a.cfg.Site)
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return errors.New("postgres did not become ready")
}

func (a *Agent) stopPG() {
	a.mu.Lock()
	cmd, exited := a.cmd, a.exited
	a.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	cmd.Process.Signal(syscall.SIGINT) // fast shutdown
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		<-exited
	}
	a.mu.Lock()
	a.cmd = nil
	a.mu.Unlock()
}

// supervise restarts postgres if it dies while we are not doing maintenance.
func (a *Agent) supervise(ctx context.Context) {
	for {
		a.mu.Lock()
		exited := a.exited
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-exited:
		}
		a.mu.Lock()
		maint := a.maint
		a.mu.Unlock()
		if maint {
			// Wait until maintenance finishes and a new process exists.
			for {
				time.Sleep(500 * time.Millisecond)
				a.mu.Lock()
				done := !a.maint && a.exited != exited
				a.mu.Unlock()
				if done || ctx.Err() != nil {
					break
				}
			}
			continue
		}
		log.Printf("agent[%s]: postgres exited unexpectedly, restarting", a.cfg.Site)
		a.setState("error", "postgres exited unexpectedly")
		time.Sleep(2 * time.Second)
		if err := a.startPG(ctx); err != nil {
			log.Printf("agent[%s]: restart failed: %v", a.cfg.Site, err)
		}
	}
}

// ---------------------------------------------------------------- operations

func (a *Agent) connect(ctx context.Context, db string) (*pgx.Conn, error) {
	cs := fmt.Sprintf("host=%s port=%d user=postgres dbname=%s sslmode=disable connect_timeout=3", a.cfg.SocketDir, a.cfg.PGPort, db)
	return pgx.Connect(ctx, cs)
}

// Status queries postgres for role and replication state.
func (a *Agent) Status(ctx context.Context) Status {
	a.mu.Lock()
	st := Status{
		Site: a.cfg.Site, State: a.state, LastErr: a.lastErr, Upstream: a.upstream,
		ChangedAt: a.changed, RebuildCount: a.rebuilds, Role: "unknown", Replicas: []Replica{},
	}
	a.mu.Unlock()
	if st.State != "running" {
		return st
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := a.connect(cctx, "postgres")
	if err != nil {
		st.LastErr = err.Error()
		return st
	}
	defer conn.Close(context.Background())
	st.Running = true
	var inRecovery bool
	var lsn, recv *string
	var lsnBytes *int64
	var readOnly, syncNames string
	err = conn.QueryRow(cctx, `
		SELECT pg_is_in_recovery(),
		       CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn()::text ELSE pg_current_wal_lsn()::text END,
		       CASE WHEN pg_is_in_recovery() THEN pg_wal_lsn_diff(pg_last_wal_replay_lsn(), '0/0')::bigint
		            ELSE pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::bigint END,
		       pg_last_wal_receive_lsn()::text,
		       (SELECT timeline_id FROM pg_control_checkpoint()),
		       current_setting('default_transaction_read_only'),
		       current_setting('synchronous_standby_names')`).
		Scan(&inRecovery, &lsn, &lsnBytes, &recv, &st.Timeline, &readOnly, &syncNames)
	if err != nil {
		st.LastErr = err.Error()
		return st
	}
	if lsn != nil {
		st.LSN = *lsn
	}
	if lsnBytes != nil {
		st.LSNBytes = *lsnBytes
	}
	if recv != nil {
		st.ReceiveLSN = *recv
	}
	st.Fenced = readOnly == "on"
	st.SyncMode = syncNames != ""
	if inRecovery {
		st.Role = "standby"
		var status *string
		if err := conn.QueryRow(cctx, `SELECT status FROM pg_stat_wal_receiver LIMIT 1`).Scan(&status); err == nil && status != nil {
			st.Streaming = *status == "streaming"
		}
		return st
	}
	st.Role = "primary"
	rows, err := conn.Query(cctx, `
		SELECT application_name, COALESCE(client_addr::text, ''), COALESCE(state, ''), COALESCE(sync_state, ''),
		       COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)::bigint, -1)
		FROM pg_stat_replication ORDER BY application_name`)
	if err == nil {
		for rows.Next() {
			var r Replica
			if rows.Scan(&r.Name, &r.Addr, &r.State, &r.Sync, &r.LagBytes) == nil {
				st.Replicas = append(st.Replicas, r)
			}
		}
		rows.Close()
	}
	return st
}

// Promote turns a standby into a primary and clears any fence.
func (a *Agent) Promote(ctx context.Context) error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	conn, err := a.connect(ctx, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var inRecovery bool
	if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return err
	}
	if inRecovery {
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT pg_promote(true, 30)").Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return errors.New("pg_promote returned false")
		}
		log.Printf("agent[%s]: promoted to primary", a.cfg.Site)
	}
	a.mu.Lock()
	a.upstream = ""
	a.changed = time.Now()
	a.mu.Unlock()
	return a.setReadOnly(ctx, conn, false)
}

// Fence makes the node refuse writes and disconnects clients. It is used on a
// former primary so that it can never accept writes that diverge from the
// promoted site (split-brain protection).
func (a *Agent) Fence(ctx context.Context, on bool) error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	conn, err := a.connect(ctx, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return a.setReadOnly(ctx, conn, on)
}

func (a *Agent) setReadOnly(ctx context.Context, conn *pgx.Conn, on bool) error {
	stmt := "ALTER SYSTEM RESET default_transaction_read_only"
	if on {
		stmt = "ALTER SYSTEM SET default_transaction_read_only = on"
	}
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		return err
	}
	if on {
		// Existing sessions keep their old setting, so disconnect them.
		_, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND backend_type = 'client backend'`)
		log.Printf("agent[%s]: fenced (read-only)", a.cfg.Site)
		return err
	}
	return nil
}

// SetSync toggles synchronous replication on a primary.
func (a *Agent) SetSync(ctx context.Context, on bool) error {
	conn, err := a.connect(ctx, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	stmt := "ALTER SYSTEM SET synchronous_standby_names = '*'"
	if !on {
		stmt = "ALTER SYSTEM RESET synchronous_standby_names"
	}
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, "SELECT pg_reload_conf()")
	return err
}

// Rebuild stops postgres, discards local data and re-clones from the given
// primary as a streaming standby. It runs in the background.
func (a *Agent) Rebuild(host string, port int) {
	go func() {
		a.opMu.Lock()
		defer a.opMu.Unlock()
		log.Printf("agent[%s]: rebuilding as standby of %s:%d", a.cfg.Site, host, port)
		a.mu.Lock()
		a.maint = true
		a.mu.Unlock()
		a.setState("rebuilding", "")
		a.stopPG()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		err := a.cloneFrom(ctx, host, port)
		if err == nil {
			err = a.startPG(ctx)
		}
		a.mu.Lock()
		a.maint = false
		a.mu.Unlock()
		if err != nil {
			a.setState("error", "rebuild failed: "+err.Error())
			log.Printf("agent[%s]: rebuild failed: %v", a.cfg.Site, err)
		}
	}()
}

func (a *Agent) readUpstream() string {
	data, err := os.ReadFile(filepath.Join(a.cfg.PGData, "postgresql.auto.conf"))
	if err != nil || !exists(filepath.Join(a.cfg.PGData, "standby.signal")) {
		return ""
	}
	var host, port string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "primary_conninfo") {
			continue
		}
		for _, f := range strings.Fields(strings.Trim(strings.SplitN(line, "=", 2)[1], " '")) {
			if v, ok := strings.CutPrefix(f, "host="); ok {
				host = v
			}
			if v, ok := strings.CutPrefix(f, "port="); ok {
				port = v
			}
		}
	}
	if host == "" {
		return ""
	}
	return host + ":" + port
}

// ---------------------------------------------------------------- API

// Handler returns the agent HTTP API.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, a.Status(r.Context()))
	})
	mux.HandleFunc("POST /promote", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		if err := a.Promote(ctx); err != nil {
			httpx.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, a.Status(ctx))
	})
	mux.HandleFunc("POST /fence", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			On *bool `json:"on"`
		}
		_ = httpx.ReadJSON(r, &body)
		on := body.On == nil || *body.On
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := a.Fence(ctx, on); err != nil {
			httpx.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, a.Status(ctx))
	})
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			On bool `json:"on"`
		}
		_ = httpx.ReadJSON(r, &body)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := a.SetSync(ctx, body.On); err != nil {
			httpx.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, a.Status(ctx))
	})
	mux.HandleFunc("POST /rebuild", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PrimaryHost string `json:"primary_host"`
			PrimaryPort int    `json:"primary_port"`
		}
		if err := httpx.ReadJSON(r, &body); err != nil || body.PrimaryHost == "" {
			httpx.Error(w, http.StatusBadRequest, "primary_host is required")
			return
		}
		if body.PrimaryPort == 0 {
			body.PrimaryPort = 5432
		}
		a.Rebuild(body.PrimaryHost, body.PrimaryPort)
		httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "rebuilding"})
	})
	return httpx.RequireToken(a.cfg.Token, mux)
}

// ---------------------------------------------------------------- helpers

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func appendFile(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s)
	return err
}

// rewriteAutoConf removes the given keys from postgresql.auto.conf and
// appends the non-empty ones with their new values.
func rewriteAutoConf(pgdata string, kv map[string]string) error {
	path := filepath.Join(pgdata, "postgresql.auto.conf")
	data, _ := os.ReadFile(path)
	var keep []string
	for _, line := range strings.Split(string(data), "\n") {
		key := strings.TrimSpace(strings.SplitN(line, "=", 2)[0])
		if _, drop := kv[key]; drop || strings.TrimSpace(line) == "" {
			continue
		}
		keep = append(keep, line)
	}
	for k, v := range kv {
		if v != "" {
			keep = append(keep, k+" = "+v)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
