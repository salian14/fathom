// Package prom is a thin client for the Prometheus HTTP API.
//
// It mirrors internal/kube in role: it owns the transport and the endpoint
// knowledge, exposes typed calls, and holds no tool logic. Everything it
// touches is read-only by construction -- the four endpoints used here query
// the TSDB and cannot write to it.
//
// The whole package is optional. fathom-mcp builds a Client only when
// FATHOM_MCP_PROMETHEUS_URL is set; with no URL there is no client, and the
// three metrics tools are never registered.
package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Client talks to one Prometheus. Safe for concurrent use.
type Client struct {
	base *url.URL
	http *http.Client
}

type Options struct {
	// BaseURL is the root of the Prometheus server, e.g.
	// "http://prometheus.observability.svc.cluster.local:9090". The /api/v1
	// prefix is added here, not by the caller.
	BaseURL string

	// Timeout bounds a single request end to end. Handlers additionally pass
	// Prometheus its own `timeout` parameter so the server stops working when
	// we stop waiting.
	Timeout time.Duration

	UserAgent string

	// Trace wraps the transport so every call becomes a client span.
	Trace bool
}

func New(opts Options) (*Client, error) {
	raw := strings.TrimSpace(opts.BaseURL)
	if raw == "" {
		return nil, fmt.Errorf("prometheus base URL is empty")
	}

	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing prometheus URL %q: %w", raw, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("prometheus URL %q must be http:// or https://", raw)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("prometheus URL %q has no host", raw)
	}
	base.Path = strings.TrimSuffix(base.Path, "/")

	var rt http.RoundTripper = &userAgentTransport{
		agent: opts.UserAgent,
		next:  http.DefaultTransport,
	}
	if opts.Trace {
		// One wrap covers every call in this package, which is the same hook
		// kube.New uses for the API server. The span name is deliberately not
		// derived from the URL: query strings carry metric names and whole
		// PromQL expressions, and a URL-derived name would make every distinct
		// query its own operation in the tracing backend.
		rt = otelhttp.NewTransport(rt,
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return "prometheus " + r.Method
			}))
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	return &Client{
		base: base,
		http: &http.Client{Transport: rt, Timeout: timeout},
	}, nil
}

// Endpoint returns the configured base URL, for logs and diagnostics.
func (c *Client) Endpoint() string { return c.base.String() }

// Ping reports whether Prometheus is answering.
//
// Not wired into /readyz on purpose: the Kubernetes tools do not depend on
// Prometheus, and coupling them would turn a metrics outage into a total one.
func (c *Client) Ping(ctx context.Context) error {
	req, err := c.request(ctx, "/-/healthy", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching prometheus at %s: %w", c.base, err)
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, Endpoint: "healthy",
			Message: "prometheus reported unhealthy"}
	}
	return nil
}

// MetricNames returns the metric names known to Prometheus, optionally
// restricted to series matching match (a PromQL selector).
func (c *Client) MetricNames(ctx context.Context, match string, limit int) ([]string, []string, error) {
	q := url.Values{}
	if match != "" {
		q.Set("match[]", match)
	}
	if limit > 0 {
		// Honoured by Prometheus 2.45+; older servers ignore it and we cap the
		// slice ourselves in the handler regardless.
		q.Set("limit", strconv.Itoa(limit))
	}

	var names []string
	warnings, err := c.get(ctx, "/api/v1/label/__name__/values", q, EndpointLabelValues, &names)
	if err != nil {
		return nil, nil, err
	}
	return names, warnings, nil
}

// Metadata returns the type, help text and unit recorded for a metric.
//
// The result is keyed by metric name and each name may carry more than one
// entry when exporters disagree; callers get the whole map and decide.
func (c *Client) Metadata(ctx context.Context, metric string) (map[string][]Meta, []string, error) {
	q := url.Values{}
	if metric != "" {
		q.Set("metric", metric)
	}

	var meta map[string][]Meta
	warnings, err := c.get(ctx, "/api/v1/metadata", q, EndpointMetadata, &meta)
	if err != nil {
		return nil, nil, err
	}
	return meta, warnings, nil
}

// Series returns the label sets of every series matching match, capped at
// limit. One call is enough to derive label names, their values, and the
// metric's cardinality.
func (c *Client) Series(ctx context.Context, match string, limit int) ([]map[string]string, []string, error) {
	q := url.Values{}
	q.Set("match[]", match)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	var series []map[string]string
	warnings, err := c.get(ctx, "/api/v1/series", q, EndpointSeries, &series)
	if err != nil {
		return nil, nil, err
	}
	return series, warnings, nil
}

// Query runs an instant PromQL query.
//
// Instant only: there is no query_range here, by design. The timeout is sent
// as an API parameter as well as bounding ctx, so Prometheus abandons the
// evaluation when we stop waiting for it rather than finishing it for nobody.
func (c *Client) Query(ctx context.Context, expr string, at time.Time, timeout time.Duration) (*QueryResult, []string, error) {
	q := url.Values{}
	q.Set("query", expr)
	if !at.IsZero() {
		q.Set("time", strconv.FormatInt(at.Unix(), 10))
	}
	if timeout > 0 {
		q.Set("timeout", timeout.String())
	}

	var result QueryResult
	warnings, err := c.get(ctx, "/api/v1/query", q, EndpointQuery, &result)
	if err != nil {
		return nil, nil, err
	}
	return &result, warnings, nil
}

// get performs a request and unwraps Prometheus's response envelope into out.
func (c *Client) get(ctx context.Context, path string, q url.Values, endpoint string, out any) ([]string, error) {
	req, err := c.request(ctx, path, q)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(err, endpoint, c.base.String())
	}
	defer drain(resp)

	var env envelope
	decodeErr := json.NewDecoder(resp.Body).Decode(&env)

	// Prometheus reports query problems as a 4xx *with* a JSON body naming the
	// errorType, so the body is worth more than the status and is read first.
	// A non-2xx with no usable body still has to surface as an error.
	if decodeErr != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, &APIError{Status: resp.StatusCode, Endpoint: endpoint,
				Message: strings.TrimSpace(resp.Status)}
		}
		return nil, fmt.Errorf("decoding prometheus %s response: %w", endpoint, decodeErr)
	}

	if env.Status != "success" {
		return nil, &APIError{
			Status:    resp.StatusCode,
			Endpoint:  endpoint,
			ErrorType: env.ErrorType,
			Message:   env.Error,
		}
	}

	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return nil, fmt.Errorf("decoding prometheus %s data: %w", endpoint, err)
		}
	}
	return env.Warnings, nil
}

func (c *Client) request(ctx context.Context, path string, q url.Values) (*http.Request, error) {
	u := *c.base
	u.Path = c.base.Path + path
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building prometheus request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// drain closes a response body. Bodies here are small and fully read by the
// decoder, so there is nothing to discard first.
func drain(resp *http.Response) { _ = resp.Body.Close() }

type userAgentTransport struct {
	agent string
	next  http.RoundTripper
}

func (t *userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.agent != "" {
		// Clone before mutating: RoundTrippers must not modify the request.
		r = r.Clone(r.Context())
		r.Header.Set("User-Agent", t.agent)
	}
	return t.next.RoundTrip(r)
}
