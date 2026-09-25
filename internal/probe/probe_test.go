package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeApp stores probe writes, can be told to fail writes, and can "lose" a
// sequence number to simulate an acknowledged write that did not survive a
// failover.
type fakeApp struct {
	mu      sync.Mutex
	seqs    map[int64]bool
	failing bool
	forget  map[int64]bool
}

func (f *fakeApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/entries":
		if f.failing {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Seq int64 `json:"seq"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if !f.forget[body.Seq] {
			f.seqs[body.Seq] = true
		}
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(r.URL.Path, "/api/seqs"):
		out := []int64{}
		for s := range f.seqs {
			out = append(out, s)
		}
		json.NewEncoder(w).Encode(map[string]any{"seqs": out})
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func TestProbeMeasuresOutageAndLostWrites(t *testing.T) {
	app := &fakeApp{seqs: map[int64]bool{}, forget: map[int64]bool{3: true}}
	srv := httptest.NewServer(app)
	defer srv.Close()
	p := New(Config{Target: srv.URL, Interval: 20 * time.Millisecond, OutageMin: 150 * time.Millisecond,
		VerifyEvery: 50 * time.Millisecond, VerifyGrace: 10 * time.Millisecond, LoadPath: "/", LoadWorkers: 4})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.writer(ctx)
	go p.verifier(ctx)

	time.Sleep(150 * time.Millisecond)
	app.mu.Lock()
	app.failing = true
	app.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	app.mu.Lock()
	app.failing = false
	app.mu.Unlock()
	time.Sleep(400 * time.Millisecond)

	st := p.Status()
	if len(st.Outages) != 1 {
		t.Fatalf("want 1 outage, got %+v", st.Outages)
	}
	if o := st.Outages[0]; o.Seconds < 0.25 || o.Failures == 0 {
		t.Fatalf("outage not measured correctly: %+v", o)
	}
	if st.Lost != 1 || st.LostSeqs[0] != 3 {
		t.Fatalf("seq 3 was acknowledged but never stored; lost=%d %v", st.Lost, st.LostSeqs)
	}
	if st.Verified == 0 {
		t.Fatal("stored writes should be verified")
	}
}

func TestResetStartsNewEpoch(t *testing.T) {
	p := New(Config{Target: "http://127.0.0.1:1", Interval: time.Second})
	id := p.Status().ClientID
	p.reset()
	if p.Status().ClientID == id || !strings.HasPrefix(p.Status().ClientID, "probe-") {
		t.Fatal("reset must start a new probe client id")
	}
}
