package cloud

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"spillway/internal/httpx"
)

// ECS bursts into an AWS ECS Fargate service that sits behind an ALB. Scaling
// sets the service desiredCount; the edge routes to the ALB with weight =
// running tasks. Requests are signed with SigV4 using static credentials from
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (/ AWS_SESSION_TOKEN).
type ECS struct {
	region, cluster, service, url string
	creds                         aws.Credentials
	signer                        *v4.Signer
	http                          *http.Client

	mu      sync.Mutex
	desired int
	running int
	lastErr string
	checked time.Time
}

// NewECSFromEnv configures the adapter from AWS_* / ECS_* variables.
func NewECSFromEnv() (*ECS, error) {
	e := &ECS{
		region:  httpx.Env("AWS_REGION", "ap-northeast-2"),
		cluster: httpx.Env("ECS_CLUSTER", "spillway"),
		service: httpx.Env("ECS_SERVICE", "spillway-app"),
		url:     httpx.Env("ECS_ENDPOINT_URL", ""),
		creds: aws.Credentials{
			AccessKeyID:     httpx.Env("AWS_ACCESS_KEY_ID", ""),
			SecretAccessKey: httpx.Env("AWS_SECRET_ACCESS_KEY", ""),
			SessionToken:    httpx.Env("AWS_SESSION_TOKEN", ""),
		},
		signer: v4.NewSigner(),
		http:   &http.Client{Timeout: 20 * time.Second},
	}
	if e.creds.AccessKeyID == "" || e.creds.SecretAccessKey == "" {
		return nil, errors.New("ecs: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required")
	}
	if e.url == "" {
		return nil, errors.New("ecs: ECS_ENDPOINT_URL (the ALB URL) is required")
	}
	return e, nil
}

func (e *ECS) Name() string { return "aws-fargate" }

func (e *ECS) call(ctx context.Context, action string, body, out any) error {
	payload, _ := json.Marshal(body)
	endpoint := fmt.Sprintf("https://ecs.%s.amazonaws.com/", e.region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AmazonEC2ContainerServiceV20141113."+action)
	sum := sha256.Sum256(payload)
	if err := e.signer.SignHTTP(ctx, e.creds, req, hex.EncodeToString(sum[:]), "ecs", e.region, time.Now()); err != nil {
		return err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ecs %s: %s: %s", action, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Scale sets the Fargate service desired task count.
func (e *ECS) Scale(ctx context.Context, n int) error {
	e.mu.Lock()
	e.desired = n
	e.mu.Unlock()
	return e.call(ctx, "UpdateService", map[string]any{
		"cluster": e.cluster, "service": e.service, "desiredCount": n,
	}, nil)
}

// Status reads running task counts (cached for 5s).
func (e *ECS) Status(ctx context.Context) Status {
	e.mu.Lock()
	stale := time.Since(e.checked) > 5*time.Second
	e.mu.Unlock()
	if stale {
		var out struct {
			Services []struct {
				DesiredCount int `json:"desiredCount"`
				RunningCount int `json:"runningCount"`
			} `json:"services"`
			Failures []struct {
				Reason string `json:"reason"`
			} `json:"failures"`
		}
		err := e.call(ctx, "DescribeServices", map[string]any{"cluster": e.cluster, "services": []string{e.service}}, &out)
		e.mu.Lock()
		e.checked = time.Now()
		switch {
		case err != nil:
			e.lastErr = err.Error()
		case len(out.Services) == 0:
			e.lastErr = "service not found"
			if len(out.Failures) > 0 {
				e.lastErr = out.Failures[0].Reason
			}
		default:
			e.lastErr = ""
			e.running = out.Services[0].RunningCount
		}
		e.mu.Unlock()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Status{Provider: e.Name(), Kind: "ecs", Desired: e.desired, Ready: e.running, Err: e.lastErr,
		Detail: fmt.Sprintf("%s/%s/%s", e.region, e.cluster, e.service), Endpoints: []Endpoint{}}
	units := e.running
	if units > e.desired {
		units = e.desired // draining tasks should not attract new traffic
	}
	st.Endpoints = append(st.Endpoints, Endpoint{Name: e.Name(), URL: e.url, Units: units})
	return st
}
