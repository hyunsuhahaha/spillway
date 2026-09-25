package siteagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteAutoConfReplacesKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.auto.conf")
	orig := "# Do not edit\nprimary_conninfo = 'host=old'\ndefault_transaction_read_only = 'on'\nwork_mem = '4MB'\n"
	os.WriteFile(path, []byte(orig), 0o600)

	err := rewriteAutoConf(dir, map[string]string{
		"primary_conninfo":              literal("host=new port=5432"),
		"default_transaction_read_only": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if strings.Contains(s, "host=old") || strings.Contains(s, "read_only") {
		t.Fatalf("old keys kept:\n%s", s)
	}
	if !strings.Contains(s, "primary_conninfo = 'host=new port=5432'") || !strings.Contains(s, "work_mem = '4MB'") {
		t.Fatalf("unexpected result:\n%s", s)
	}
}

func TestQuoting(t *testing.T) {
	if got := literal("it's"); got != "'it''s'" {
		t.Fatalf("literal = %s", got)
	}
	if got := ident(`we"ird`); got != `"we""ird"` {
		t.Fatalf("ident = %s", got)
	}
}

func TestReadUpstream(t *testing.T) {
	dir := t.TempDir()
	a := New(Config{PGData: dir})
	os.WriteFile(filepath.Join(dir, "postgresql.auto.conf"), []byte("primary_conninfo = 'host=cloud-db port=5433 user=r'\n"), 0o600)
	if got := a.readUpstream(); got != "" {
		t.Fatalf("no standby.signal: want empty, got %q", got)
	}
	os.WriteFile(filepath.Join(dir, "standby.signal"), nil, 0o600)
	if got := a.readUpstream(); got != "cloud-db:5433" {
		t.Fatalf("upstream = %q", got)
	}
}
