package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"spillway/internal/httpx"
)

// Docker bursts into containers on the local Docker engine. It stands in for
// a cloud provider in the single-machine simulation and uses the Engine API
// directly over the unix socket (no SDK).
type Docker struct {
	name    string
	image   string
	network string
	prefix  string
	cmd     []string
	env     []string
	port    int
	nanoCPU int64
	http    *http.Client

	mu      sync.Mutex
	desired int
}

// NewDockerFromEnv configures the adapter from DOCKER_* / BURST_* variables.
func NewDockerFromEnv() *Docker {
	sock := httpx.Env("DOCKER_SOCKET", "/var/run/docker.sock")
	cpus := httpx.EnvFloat("BURST_CPUS", 0)
	var env []string
	for _, kv := range strings.Split(httpx.Env("BURST_ENV", ""), ";") {
		if kv = strings.TrimSpace(kv); kv != "" {
			env = append(env, kv)
		}
	}
	return &Docker{
		name:    httpx.Env("DOCKER_PROVIDER_NAME", "sim-cloud"),
		image:   httpx.Env("BURST_IMAGE", "spillway:latest"),
		network: httpx.Env("BURST_NETWORK", "spillway-sim_wan"),
		prefix:  httpx.Env("BURST_PREFIX", "spillway-burst"),
		cmd:     strings.Fields(httpx.Env("BURST_CMD", "app")),
		env:     env,
		port:    httpx.EnvInt("BURST_PORT", 8080),
		nanoCPU: int64(cpus * 1e9),
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			}},
		},
	}
}

func (d *Docker) Name() string { return d.name }

type dockerContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

func (d *Docker) api(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		return fmt.Errorf("docker %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (d *Docker) list(ctx context.Context) (map[int]dockerContainer, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {"spillway.burst=" + d.prefix}})
	var cs []dockerContainer
	if err := d.api(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil, &cs); err != nil {
		return nil, err
	}
	out := map[int]dockerContainer{}
	for _, c := range cs {
		if idx, err := strconv.Atoi(c.Labels["spillway.index"]); err == nil {
			out[idx] = c
		}
	}
	return out, nil
}

func (d *Docker) containerName(i int) string { return fmt.Sprintf("%s-%d", d.prefix, i) }

// Scale creates or removes burst containers so exactly n exist.
func (d *Docker) Scale(ctx context.Context, n int) error {
	d.mu.Lock()
	d.desired = n
	d.mu.Unlock()
	cur, err := d.list(ctx)
	if err != nil {
		return err
	}
	var errs []string
	var wg sync.WaitGroup
	var emu sync.Mutex
	fail := func(err error) {
		emu.Lock()
		errs = append(errs, err.Error())
		emu.Unlock()
	}
	for i := 0; i < n; i++ {
		c, ok := cur[i]
		if ok && c.State == "running" {
			continue
		}
		wg.Add(1)
		go func(i int, c dockerContainer, ok bool) {
			defer wg.Done()
			if ok {
				if err := d.api(ctx, http.MethodPost, "/containers/"+c.ID+"/start", nil, nil); err != nil {
					fail(err)
				}
				return
			}
			if err := d.create(ctx, i); err != nil {
				fail(err)
			}
		}(i, c, ok)
	}
	for idx, c := range cur {
		if idx < n {
			continue
		}
		wg.Add(1)
		go func(c dockerContainer) {
			defer wg.Done()
			d.api(ctx, http.MethodPost, "/containers/"+c.ID+"/stop?t=3", nil, nil)
			if err := d.api(ctx, http.MethodDelete, "/containers/"+c.ID+"?force=1", nil, nil); err != nil {
				fail(err)
			}
		}(c)
	}
	wg.Wait()
	if len(errs) > 0 {
		return fmt.Errorf("docker scale: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (d *Docker) create(ctx context.Context, i int) error {
	name := d.containerName(i)
	env := append([]string{"INSTANCE=" + name}, d.env...)
	body := map[string]any{
		"Image":  d.image,
		"Cmd":    d.cmd,
		"Env":    env,
		"Labels": map[string]string{"spillway.burst": d.prefix, "spillway.index": strconv.Itoa(i)},
		"HostConfig": map[string]any{
			"NetworkMode": d.network,
			"Init":        true,
			"NanoCpus":    d.nanoCPU,
		},
	}
	var created struct {
		ID string `json:"Id"`
	}
	// A stale container with the same name may exist; remove it first.
	d.api(ctx, http.MethodDelete, "/containers/"+name+"?force=1", nil, nil)
	if err := d.api(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body, &created); err != nil {
		return err
	}
	return d.api(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil)
}

// Status lists running burst containers as endpoints.
func (d *Docker) Status(ctx context.Context) Status {
	d.mu.Lock()
	st := Status{Provider: d.name, Kind: "docker", Desired: d.desired, Endpoints: []Endpoint{}}
	d.mu.Unlock()
	cur, err := d.list(ctx)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	idx := make([]int, 0, len(cur))
	for i := range cur {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		if cur[i].State != "running" {
			continue
		}
		st.Ready++
		name := d.containerName(i)
		st.Endpoints = append(st.Endpoints, Endpoint{Name: name, URL: fmt.Sprintf("http://%s:%d", name, d.port), Units: 1})
	}
	st.Detail = fmt.Sprintf("image %s on network %s", d.image, d.network)
	return st
}
