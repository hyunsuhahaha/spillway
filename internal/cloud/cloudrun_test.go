package cloud

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCloudRunReportsObservedSeparatelyFromConfiguredMinimum(t *testing.T) {
	t.Setenv("GCP_ACCESS_TOKEN", "test-token")
	at := time.Now().UTC().Truncate(time.Second)
	c := &CloudRun{project: "demo", region: "asia-northeast3", service: "spillway-app", desired: 2,
		ready: true, checked: time.Now()}
	c.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "monitoring.googleapis.com" || r.URL.Query().Get("view") != "FULL" ||
			!strings.Contains(r.URL.Query().Get("filter"), `resource.labels.service_name="spillway-app"`) {
			t.Errorf("unexpected monitoring request: %s", r.URL)
		}
		body := fmt.Sprintf(`{"timeSeries":[{"points":[{"interval":{"endTime":%q},"value":{"int64Value":"3"}}]},{"points":[{"interval":{"endTime":%q},"value":{"int64Value":"1"}}]}]}`, at.Format(time.RFC3339), at.Format(time.RFC3339))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	st := c.Status(t.Context())
	if st.Ready != 2 || st.Observed == nil || *st.Observed != 4 || st.ObservedAt == "" {
		t.Fatalf("configured minimum and observed count were conflated: %+v", st)
	}
}

func TestCloudRunMissingMetricIsUnknownNotZero(t *testing.T) {
	t.Setenv("GCP_ACCESS_TOKEN", "test-token")
	c := &CloudRun{project: "demo", region: "asia-northeast3", service: "spillway-app", checked: time.Now()}
	c.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"timeSeries":[]}`)), Header: make(http.Header)}, nil
	})}
	if st := c.Status(t.Context()); st.Observed != nil {
		t.Fatalf("missing metric must not be reported as zero: %+v", st)
	}
}
