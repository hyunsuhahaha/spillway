package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

	mu       sync.Mutex
	desired  int
	token    string
	tokenExp time.Time
	uri      string
	ready    bool
	lastErr  string
	checked  time.Time
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
	c.mu.Lock()
	c.desired = n
	c.mu.Unlock()
	err := c.call(ctx, http.MethodPatch, c.serviceURL()+"?updateMask=scaling.minInstanceCount",
		map[string]any{"scaling": map[string]any{"minInstanceCount": n}}, nil)
	if err == nil {
		return nil
	}
	var se *httpx.StatusError
	if errors.As(err, &se) && se.Code == http.StatusBadRequest {
		return c.call(ctx, http.MethodPatch, c.serviceURL()+"?updateMask=template.scaling.minInstanceCount",
			map[string]any{"template": map[string]any{"scaling": map[string]any{"minInstanceCount": n}}}, nil)
	}
	return err
}

// Status reads the service URL and readiness (cached for 5s).
func (c *CloudRun) Status(ctx context.Context) Status {
	c.mu.Lock()
	stale := time.Since(c.checked) > 5*time.Second
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
	c.mu.Lock()
	defer c.mu.Unlock()
	url := c.url
	if url == "" {
		url = c.uri
	}
	st := Status{Provider: c.Name(), Kind: "cloudrun", Desired: c.desired, Err: c.lastErr, Endpoints: []Endpoint{},
		Detail: fmt.Sprintf("%s/%s/%s", c.project, c.region, c.service)}
	if c.ready || c.url != "" {
		st.Ready = c.desired
	}
	if url != "" {
		st.Endpoints = append(st.Endpoints, Endpoint{Name: c.Name(), URL: url, Units: c.desired})
	}
	return st
}
