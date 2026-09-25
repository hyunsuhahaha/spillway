package metrics

import (
	"testing"
	"time"
)

func TestStatsPercentilesAndGroups(t *testing.T) {
	w := NewWindow(10 * time.Second)
	now := time.Now()
	for i := 1; i <= 100; i++ {
		w.Add(Sample{At: now, Dur: time.Duration(i) * time.Millisecond, OK: i != 100, Group: "local"})
	}
	w.Add(Sample{At: now, Dur: 500 * time.Millisecond, OK: true, Group: "cloud"})
	st := w.Stats("local")
	if st.Count != 100 || st.P50 != 50 || st.P95 != 95 || st.P99 != 99 || st.Errors != 1 {
		t.Fatalf("unexpected stats %+v", st)
	}
	if st.RPS != 10 {
		t.Fatalf("rps = %v", st.RPS)
	}
	if all := w.Stats(""); all.Count != 101 {
		t.Fatalf("all count = %d", all.Count)
	}
}

func TestOldSamplesArePruned(t *testing.T) {
	w := NewWindow(time.Second)
	w.Add(Sample{At: time.Now().Add(-5 * time.Second), Dur: time.Second, OK: true})
	w.Add(Sample{At: time.Now(), Dur: time.Millisecond, OK: true})
	if st := w.Stats(""); st.Count != 1 || st.P95 != 1 {
		t.Fatalf("old sample not pruned: %+v", st)
	}
}
