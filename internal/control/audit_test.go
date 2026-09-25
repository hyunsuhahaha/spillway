package control

import (
	"path/filepath"
	"testing"
)

func TestAuditLogSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	c := New(Config{EventLog: path})
	c.loadAudit() // creates the directory
	c.event("action", "긴급 대피 시작")
	r := c.beginOp("evacuate", "test")
	r.step("클라우드 DB 승격", func() error { return nil })
	r.finish()

	restarted := New(Config{EventLog: path})
	restarted.loadAudit()
	if len(restarted.events) != 1 || restarted.events[0].Msg != "긴급 대피 시작" {
		t.Fatalf("events not restored: %+v", restarted.events)
	}
	if len(restarted.operations) != 1 || restarted.curOp == nil || restarted.curOp.Steps[0].Name != "클라우드 DB 승격" {
		t.Fatalf("operation not restored: %+v", restarted.operations)
	}
}
