// Package cloud contains the burst-capacity adapters. Each adapter hides one
// provider's native scaling API behind the same small interface, so the
// control plane can burst into GCP Cloud Run, AWS Fargate or (for local
// simulation) plain Docker containers without knowing the difference.
package cloud

import (
	"context"
	"fmt"
	"strings"

	"spillway/internal/httpx"
)

// Endpoint is one routable burst backend.
type Endpoint struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Units int    `json:"units"`
}

// Status describes the provider's current capacity.
type Status struct {
	Provider  string     `json:"provider"`
	Kind      string     `json:"kind"`
	Desired   int        `json:"desired"`
	Ready     int        `json:"ready"`
	Endpoints []Endpoint `json:"endpoints"`
	Detail    string     `json:"detail,omitempty"`
	Err       string     `json:"err,omitempty"`
}

// Provider scales burst capacity on one platform.
type Provider interface {
	Name() string
	// Scale sets the desired number of instances (0 = scale to zero).
	Scale(ctx context.Context, n int) error
	// Status reports desired/ready instances and the endpoints to route to.
	Status(ctx context.Context) Status
}

// FromEnv builds the providers listed in CLOUD_PROVIDERS (comma separated:
// docker, cloudrun, ecs).
func FromEnv() ([]Provider, error) {
	var out []Provider
	for _, kind := range strings.Split(httpx.Env("CLOUD_PROVIDERS", "docker"), ",") {
		switch strings.TrimSpace(kind) {
		case "":
		case "docker":
			out = append(out, NewDockerFromEnv())
		case "cloudrun":
			p, err := NewCloudRunFromEnv()
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		case "ecs":
			p, err := NewECSFromEnv()
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		default:
			return nil, fmt.Errorf("unknown cloud provider %q", kind)
		}
	}
	return out, nil
}

// Split distributes n instances across k providers as evenly as possible,
// giving the remainder to the first providers.
func Split(n, k int) []int {
	out := make([]int, k)
	if k == 0 {
		return out
	}
	for i := range out {
		out[i] = n / k
		if i < n%k {
			out[i]++
		}
	}
	return out
}
