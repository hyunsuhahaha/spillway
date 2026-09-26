package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"spillway/internal/httpx"
)

// CloudRun bursts into a Google Cloud Run service. Cloud Run autoscales on
// request load by itself; Spillway sets the service-level minimum instance
// count so capacity is warm before traffic is shifted, and back to zero when
// the burst ends. The edge routes to the service URL with weight = instances.
//
// Auth: an OAuth token from the GCE metadata server (the control plane runs on
// the anchor VM) or GCP_ACCESS_TOKEN for testing.
type CloudRun struct {
	project, region, service, url string
	http                          *http.Client

	mu              sync.Mutex
	desired         int
	token           string
	tokenExp        time.Time
	uri             string
	ready           bool
	lastErr         string
	checked         time.Time
	observed        *int
	observedAt      time.Time
	observedChecked time.Time
	observedErr     string
}

// NewCloudRunFromEnv configures the adapter from GCP_* / CLOUDRUN_* variables.
func NewCloudRunFromEnv() (*CloudRun, error) {
	c := &CloudRun{
		project: httpx.Env("GCP_PROJECT", ""),
		region:  httpx.Env("GCP_REGION", "asia-northeast3"),
		service: httpx.Env("CLOUDRUN_SERVICE", "spillway-app"),
		url:     httpx.Env("CLOUDRUN_URL", ""),
		http:    &http.Client{Timeout: 20 * time.Second},
	}
	if c.project == "" {
		return nil, errors.New("cloudrun: GCP_PROJECT is required")
	}
	return c, nil
}

func (c *CloudRun) Name() string { return "gcp-cloudrun" }

func (c *CloudRun) serviceURL() string {
	return fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/services/%s", c.project, c.region, c.service)
}

func (c *CloudRun) accessToken(ctx context.Context) (string, error) {
	if t := httpx.Env("GCP_ACCESS_TOKEN", ""); t != "" {
		return t, nil
	}
	c.mu.Lock()
	if c.token != "" && time.Until(c.tokenExp) > time.Minute {
		t := c.token
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata token: %w", err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("metadata token: %v (%s)", err, resp.Status)
	}
	c.mu.Lock()
	c.token, c.tokenExp = tok.AccessToken, time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second)
	c.mu.Unlock()
	return tok.AccessToken, nil
}

func (c *CloudRun) call(ctx context.Context, method, url string, body, out any) error {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	cl := &httpx.Client{HTTP: c.http}
	return cl.DoWithHeaders(ctx, method, url, map[string]string{"Authorization": "Bearer " + tok}, body, out)
}

// Scale sets the service-level minimum instances (no new revision needed).
// If the API rejects the service-level field it falls back to the revision
// template, which deploys a new revision.
func (c *CloudRun) Scale(ctx context.Context, n int) error {
	err := c.call(ctx, http.MethodPatch, c.serviceURL()+"?updateMask=scaling.minInstanceCount",
		map[string]any{"scaling": map[string]any{"minInstanceCount": n}}, nil)
	var se *httpx.StatusError
	if errors.As(err, &se) && se.Code == http.StatusBadRequest {
		err = c.call(ctx, http.MethodPatch, c.serviceURL()+"?updateMask=template.scaling.minInstanceCount",
			map[string]any{"template": map[string]any{"scaling": map[string]any{"minInstanceCount": n}}}, nil)
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.desired = n
	c.checked = time.Time{} // verify the new service state on the next poll
	c.mu.Unlock()
	return nil
}

// Status reads the service URL and readiness (cached for 5s).
func (c *CloudRun) Status(ctx context.Context) Status {
	c.mu.Lock()
	stale := time.Since(c.checked) > 5*time.Second
	observe := time.Since(c.observedChecked) > time.Minute
	c.mu.Unlock()
	if stale {
		var svc struct {
			URI               string `json:"uri"`
			TerminalCondition struct {
				State string `json:"state"`
			} `json:"terminalCondition"`
		}
		err := c.call(ctx, http.MethodGet, c.serviceURL(), nil, &svc)
		c.mu.Lock()
		c.checked = time.Now()
		if err != nil {
			c.lastErr = err.Error()
		} else {
			c.lastErr = ""
			c.uri = svc.URI
			c.ready = svc.TerminalCondition.State == "CONDITION_SUCCEEDED"
		}
		c.mu.Unlock()
	}
	if observe {
		count, at, err := c.instanceCount(ctx)
		c.mu.Lock()
		c.observedChecked = time.Now()
		if err != nil {
			c.observed = nil
			c.observedErr = err.Error()
		} else {
			c.observed = count
			c.observedAt = at
			c.observedErr = ""
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	url := c.url
	if url == "" {
		url = c.uri
	}
	st := Status{Provider: c.Name(), Kind: "cloudrun", Desired: c.desired, Err: c.lastErr, Endpoints: []Endpoint{},
		Detail: fmt.Sprintf("%s/%s/%s", c.project, c.region, c.service)}
	if c.observed != nil {
		count := *c.observed
		st.Observed = &count
		st.ObservedAt = c.observedAt.Format(time.RFC3339)
	}
	st.ObservedErr = c.observedErr
	if c.ready || c.url != "" {
		// This is the configured warm minimum, not an observed instance count.
		st.Ready = c.desired
	}
	if url != "" {
		st.Endpoints = append(st.Endpoints, Endpoint{Name: c.Name(), URL: url, Units: c.desired})
	}
	return st
}

// instanceCount reads the sampled Cloud Run instance gauge. Monitoring samples
// every 60s and can publish up to 120s later, so this is for display only;
// routing readiness continues to come from the edge's direct health checks.
func (c *CloudRun) instanceCount(ctx context.Context) (*int, time.Time, error) {
	now := time.Now().UTC()
	q := url.Values{}
	q.Set("filter", fmt.Sprintf(`metric.type="run.googleapis.com/container/instance_count" AND resource.type="cloud_run_revision" AND resource.labels.service_name="%s" AND resource.labels.location="%s"`, c.service, c.region))
	q.Set("interval.startTime", now.Add(-5*time.Minute).Format(time.RFC3339))
	q.Set("interval.endTime", now.Format(time.RFC3339))
	q.Set("view", "FULL")
	q.Set("pageSize", "1000")
	var result struct {
		TimeSeries []struct {
			Points []struct {
				Interval struct {
					EndTime time.Time `json:"endTime"`
				} `json:"interval"`
				Value struct {
					Int64Value string `json:"int64Value"`
				} `json:"value"`
			} `json:"points"`
		} `json:"timeSeries"`
		NextPageToken string `json:"nextPageToken"`
	}
	endpoint := "https://monitoring.googleapis.com/v3/projects/" + c.project + "/timeSeries?" + q.Encode()
	if err := c.call(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return nil, time.Time{}, err
	}
	if result.NextPageToken != "" {
		return nil, time.Time{}, errors.New("instance metric exceeded one page")
	}
	latest := time.Time{}
	for _, series := range result.TimeSeries {
		if len(series.Points) > 0 && series.Points[0].Interval.EndTime.After(latest) {
			latest = series.Points[0].Interval.EndTime
		}
	}
	if latest.IsZero() || now.Sub(latest) > 4*time.Minute {
		return nil, time.Time{}, nil // no recent sample is not proof of zero
	}
	count := 0
	for _, series := range result.TimeSeries {
		if len(series.Points) == 0 || latest.Sub(series.Points[0].Interval.EndTime) > 90*time.Second {
			continue
		}
		n, err := strconv.Atoi(series.Points[0].Value.Int64Value)
		if err != nil || n < 0 {
			return nil, time.Time{}, fmt.Errorf("invalid instance metric value %q", series.Points[0].Value.Int64Value)
		}
		count += n
	}
	return &count, latest, nil
}
