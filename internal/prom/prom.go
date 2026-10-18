// idlerthing — a lightweight, self-hosted inventory for hosting services.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// Package prom is a minimal Prometheus HTTP API client for live metrics.
package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client queries a Prometheus server's instant-query API.
type Client struct {
	// BaseURL is injectable for tests.
	BaseURL string
	// HTTPClient defaults to a 5s-timeout client when nil.
	HTTPClient *http.Client
}

// New creates a client for baseURL (e.g. "http://prometheus:9090").
func New(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		HTTPClient: defaultClient(baseURL),
	}
}

// defaultClient is the client used when the caller did not inject one.
// Redirects must stay on the configured origin: the query string carries
// PromQL (which names internal scrape targets), and a base URL that reaches
// Prometheus through HTTP basic auth (https://user:pass@host) would otherwise
// hand both the credential and the query to whatever host answers a redirect.
func defaultClient(baseURL string) *http.Client {
	hc := &http.Client{Timeout: 5 * time.Second}
	origin, err := url.Parse(baseURL)
	if err != nil || origin.Host == "" {
		return hc
	}
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != origin.Host || req.URL.Scheme != origin.Scheme {
			return fmt.Errorf("redirect to %s://%s refused; endpoint is %s://%s",
				req.URL.Scheme, req.URL.Host, origin.Scheme, origin.Host)
		}
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return hc
}

// transportError reduces a client transport failure to a display-safe error.
// A *url.Error prints the whole request URL first, so returning it verbatim
// would put an operator's basic-auth credential into the log and the settings
// flash; the unwrapped cause still says what happened (DNS, refused, timeout).
func transportError(baseURL string, err error) error {
	host := "endpoint"
	if u, uerr := url.Parse(baseURL); uerr == nil && u.Host != "" {
		host = u.Host
	}
	var ue *url.Error
	cause := err
	if errors.As(err, &ue) && ue.Err != nil {
		cause = ue.Err
	}
	return fmt.Errorf("connect to %s: %w", host, cause)
}

// clip bounds a remote server's error text for display.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Sample is one instant-vector result: metric labels + value.
type Sample struct {
	Metric map[string]string
	Value  float64
}

// queryResponse mirrors the Prometheus /api/v1/query envelope.
type queryResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Query runs an instant query and returns the result vector.
func (c *Client) Query(ctx context.Context, promql string) ([]Sample, error) {
	hc := c.HTTPClient
	if hc == nil {
		hc = defaultClient(c.BaseURL)
	}
	u := c.BaseURL + "/api/v1/query?query=" + url.QueryEscape(promql)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, transportError(c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned %d", resp.StatusCode)
	}
	var body queryResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if body.Status != "success" {
		msg := body.Error
		if msg == "" {
			msg = body.Status
		}
		return nil, fmt.Errorf("query failed: %s", clip(msg, 200))
	}
	var out []Sample
	for _, r := range body.Data.Result {
		if len(r.Value) != 2 {
			out = append(out, Sample{Metric: r.Metric})
			continue
		}
		s, ok := r.Value[1].(string)
		if !ok {
			out = append(out, Sample{Metric: r.Metric})
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue // "NaN"/"+Inf" samples carry no information — skip them
		}
		out = append(out, Sample{Metric: r.Metric, Value: v})
	}
	return out, nil
}

// HostMetrics holds the live numbers for one machine. OnlineKnown is false
// when the `up` query itself failed — Online is then UNKNOWN, not down
// (callers must not render a red "down" dot next to live CPU numbers).
type HostMetrics struct {
	Instance       string
	Found          bool
	Online         bool
	OnlineKnown    bool
	CPUPct         float64
	RAMPct         float64
	DiskPct        float64
	NetRxBps       float64
	NetTxBps       float64
	Uptime30d      float64
	Uptime30dValid bool
}

// Metrics bundles everything ServerMetrics fetched: lookup by nodename
// (matching key) and by instance. Healthy reports whether at least one
// query succeeded — distinguishes "prometheus down" from "zero targets".
type Metrics struct {
	ByNodename map[string]*HostMetrics
	ByInstance map[string]*HostMetrics
	Healthy    bool
}

// Instant queries used by ServerMetrics.
var serverQueries = map[string]string{
	"up":     `up`,
	"uname":  `node_uname_info`,
	"cpu":    `100 - (avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)`,
	"ram":    `100 * (1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes))`,
	"disk":   `100 * (1 - (node_filesystem_avail_bytes{mountpoint="/",fstype!~"tmpfs|overlay"} / node_filesystem_size_bytes{mountpoint="/",fstype!~"tmpfs|overlay"}))`,
	"net_rx": `sum by (instance) (rate(node_network_receive_bytes_total{device!~"lo|veth.*|docker.*"}[5m]))`,
	"net_tx": `sum by (instance) (rate(node_network_transmit_bytes_total{device!~"lo|veth.*|docker.*"}[5m]))`,
	"uptime": `avg_over_time(up[30d:15m]) * 100`,
}

// queryBatch runs queries with bounded parallelism (3 in flight). Individual
// failures are tolerated — whatever answered is returned — but the first error
// comes back with the counts so a caller can say WHY a batch came up thin.
// Without that, an unreachable Prometheus is indistinguishable from a server
// that exports nothing.
func (c *Client) queryBatch(ctx context.Context, queries map[string]string) (map[string][]Sample, error) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	var mu sync.Mutex
	out := map[string][]Sample{}
	var firstErr error
	failed := 0
	for key, ql := range queries {
		wg.Add(1)
		sem <- struct{}{}
		go func(key, ql string) {
			defer wg.Done()
			defer func() { <-sem }()
			samples, err := c.Query(ctx, ql)
			if err != nil {
				mu.Lock()
				failed++
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", key, err)
				}
				mu.Unlock()
				return // tolerate
			}
			mu.Lock()
			out[key] = samples
			mu.Unlock()
		}(key, ql)
	}
	wg.Wait()
	if firstErr != nil {
		firstErr = fmt.Errorf("%w (%d of %d queries failed)", firstErr, failed, len(queries))
	}
	return out, firstErr
}

// ServerMetrics batch-fetches all host metrics in parallel (shared 8s
// deadline across the batch, ~3 queries in flight at a time). Individual
// query failures are tolerated — whatever was fetched is returned.
func (c *Client) ServerMetrics(ctx context.Context) *Metrics {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	m := &Metrics{
		ByNodename: map[string]*HostMetrics{},
		ByInstance: map[string]*HostMetrics{},
	}

	var mu sync.Mutex
	hostFor := func(instance string) *HostMetrics {
		mu.Lock()
		defer mu.Unlock()
		h, ok := m.ByInstance[instance]
		if !ok {
			h = &HostMetrics{Instance: instance}
			m.ByInstance[instance] = h
		}
		return h
	}

	results, batchErr := c.queryBatch(ctx, serverQueries)
	if batchErr != nil {
		// Once per batch at most: the web layer caches and backs off, so a
		// dead endpoint logs every 30s rather than on every page render.
		slog.Warn("prometheus query batch failed", "endpoint", c.BaseURL, "err", batchErr)
	}

	apply := func(key string, fn func(Sample)) {
		samples, ok := results[key]
		if !ok {
			return // tolerate: leave those fields at zero values
		}
		m.Healthy = true
		for _, s := range samples {
			fn(s)
		}
	}

	apply("uname", func(s Sample) {
		instance := s.Metric["instance"]
		nodename := s.Metric["nodename"]
		if instance == "" || nodename == "" {
			return
		}
		m.ByNodename[nodename] = hostFor(instance)
	})
	apply("up", func(s Sample) {
		h := hostFor(s.Metric["instance"])
		h.Found = true
		h.Online = s.Value == 1
		h.OnlineKnown = true
	})
	apply("cpu", func(s Sample) { hostFor(s.Metric["instance"]).CPUPct = s.Value })
	apply("ram", func(s Sample) { hostFor(s.Metric["instance"]).RAMPct = s.Value })
	apply("disk", func(s Sample) { hostFor(s.Metric["instance"]).DiskPct = s.Value })
	apply("net_rx", func(s Sample) { hostFor(s.Metric["instance"]).NetRxBps = s.Value })
	apply("net_tx", func(s Sample) { hostFor(s.Metric["instance"]).NetTxBps = s.Value })
	apply("uptime", func(s Sample) {
		h := hostFor(s.Metric["instance"])
		h.Uptime30d = s.Value
		h.Uptime30dValid = true
	})

	// Mark hosts known via uname as found even when up is missing.
	for _, h := range m.ByNodename {
		if h.Found {
			continue
		}
		// Only mark found if we have any signal at all.
		if h.Online || h.CPUPct != 0 || h.RAMPct != 0 {
			h.Found = true
		}
	}
	return m
}

// UptimePct returns the 30-day uptime percentage for one instance. The 15m
// subquery step keeps it cheap (full-resolution 30d range vectors can take
// several seconds against busy Prometheus servers and blow the client
// timeout); 15m sampling is plenty for an uptime percentage.
func (c *Client) UptimePct(ctx context.Context, instance string) (float64, error) {
	samples, err := c.Query(ctx,
		`avg_over_time(up{instance=`+strconv.Quote(instance)+`}[30d:15m]) * 100`)
	if err != nil {
		return 0, err
	}
	if len(samples) == 0 {
		return 0, fmt.Errorf("no data")
	}
	return samples[0].Value, nil
}
