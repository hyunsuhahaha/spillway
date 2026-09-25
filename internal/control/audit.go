package control

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// auditRecord is one line of the JSON-lines audit log. Every event and every
// finished operation (with its timed steps) is appended, so the history of
// bursts, evacuations and failbacks survives a control-plane restart.
type auditRecord struct {
	Type      string     `json:"type"` // event | operation
	Event     *Event     `json:"event,omitempty"`
	Operation *Operation `json:"operation,omitempty"`
}

func (c *Controller) appendAudit(rec auditRecord) {
	if c.cfg.EventLog == "" {
		return
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(c.cfg.EventLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		log.Printf("control: audit log: %v", err)
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

// loadAudit restores recent events and operations from the audit log.
func (c *Controller) loadAudit() {
	if c.cfg.EventLog == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.cfg.EventLog), 0o750); err != nil {
		log.Printf("control: audit log dir: %v", err)
	}
	f, err := os.Open(c.cfg.EventLog)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var rec auditRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		switch {
		case rec.Event != nil:
			c.events = append(c.events, *rec.Event)
		case rec.Operation != nil:
			c.operations = append(c.operations, *rec.Operation)
		}
	}
	if len(c.events) > 300 {
		c.events = c.events[len(c.events)-300:]
	}
	if len(c.operations) > 20 {
		c.operations = c.operations[len(c.operations)-20:]
	}
	if n := len(c.operations); n > 0 {
		op := c.operations[n-1]
		c.curOp = &op
	}
	log.Printf("control: restored %d events and %d operations from %s", len(c.events), len(c.operations), c.cfg.EventLog)
}
