package prom

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestClient wires a Client to a handler standing in for Prometheus.
//
// httptest is used here and nowhere else in this repo for a reason: this is
// the one place fathom-mcp speaks to a foreign HTTP API whose envelope it has
// to decode. The Kubernetes tools are unit-tested at the policy layer with a
// nil client, because their decisions all happen before a request is made --
// these decisions happen after the response comes back.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New(Options{BaseURL: srv.URL, Timeout: 2 * time.Second, UserAgent: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRejectsUnusableURLs(t *testing.T) {
	for _, bad := range []string{"", "   ", "prometheus:9090", "ftp://p:9090", "http://"} {
		if _, err := New(Options{BaseURL: bad}); err == nil {
			t.Errorf("New(%q) = nil error, want a refusal", bad)
		}
	}
	if _, err := New(Options{BaseURL: "http://p:9090/prefix/"}); err != nil {
		t.Errorf("a URL with a path prefix was rejected: %v", err)
	}
}

func TestMetricNamesDecodesAndPassesFilters(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if got := r.URL.Path; got != "/api/v1/label/__name__/values" {
			t.Errorf("path = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":["up","node_cpu_seconds_total"]}`))
	})

	names, warnings, err := c.MetricNames(context.Background(), `{job="kubelet"}`, 10)
	if err != nil {
		t.Fatalf("MetricNames: %v", err)
	}
	if len(names) != 2 || names[0] != "up" {
		t.Errorf("names = %v", names)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if gotQuery != `limit=10&match%5B%5D=%7Bjob%3D%22kubelet%22%7D` {
		t.Errorf("query = %q; the selector and limit must reach prometheus", gotQuery)
	}
}

// A malformed query is the failure mode that matters most: the model has to be
// able to read Prometheus's verdict and fix its own query on the next turn.
func TestQueryPropagatesBadDataVerbatim(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data",` +
			`"error":"parse error at char 8: unexpected \")\""}`))
	})

	_, _, err := c.Query(context.Background(), "sum by (", time.Time{}, time.Second)
	if err == nil {
		t.Fatal("Query on a malformed expression returned no error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.ErrorType != "bad_data" {
		t.Errorf("ErrorType = %q, want bad_data", apiErr.ErrorType)
	}
	if apiErr.Message != `parse error at char 8: unexpected ")"` {
		t.Errorf("prometheus's own wording was lost: %q", apiErr.Message)
	}
}

func TestQueryDecodesVectorAndWarnings(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","warnings":["partial results"],` +
			`"data":{"resultType":"vector","result":[` +
			`{"metric":{"__name__":"up","job":"kubelet"},"value":[1755432000.5,"1"]}]}}`))
	})

	result, warnings, err := c.Query(context.Background(), "up", time.Time{}, time.Second)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(warnings) != 1 || warnings[0] != "partial results" {
		t.Errorf("warnings = %v; a partial result the caller is not told about reads as complete", warnings)
	}

	samples, err := result.AsVector()
	if err != nil {
		t.Fatalf("AsVector: %v", err)
	}
	if len(samples) != 1 || samples[0].Value.Value != "1" {
		t.Fatalf("samples = %+v", samples)
	}
	if got := samples[0].Value.Time().Format(time.RFC3339); got != "2025-08-17T12:00:00Z" {
		t.Errorf("sample time = %q", got)
	}
	if samples[0].Metric["job"] != "kubelet" {
		t.Errorf("labels lost: %v", samples[0].Metric)
	}
}

// Values stay strings all the way through. Prometheus sends them that way so
// NaN, +Inf and full float64 precision survive, and parsing to a float here
// would quietly lose all three.
func TestSpecialValuesSurvive(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{},"value":[1,"NaN"]},{"metric":{},"value":[2,"+Inf"]}]}}`))
	})

	result, _, err := c.Query(context.Background(), "x", time.Time{}, 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	samples, err := result.AsVector()
	if err != nil {
		t.Fatalf("AsVector: %v", err)
	}
	if samples[0].Value.Value != "NaN" || samples[1].Value.Value != "+Inf" {
		t.Errorf("special values mangled: %+v", samples)
	}
}

// A 5xx with no JSON body at all still has to surface as an error rather than
// as an empty success.
func TestServerErrorWithoutBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, _, err := c.MetricNames(context.Background(), "", 0)
	if err == nil {
		t.Fatal("a 503 with an empty body was treated as success")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want 503", apiErr.Status)
	}
}

func TestSeriesAndMetadataDecode(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/series":
			_, _ = w.Write([]byte(`{"status":"success","data":[` +
				`{"__name__":"up","job":"kubelet"},{"__name__":"up","job":"api"}]}`))
		case "/api/v1/metadata":
			_, _ = w.Write([]byte(`{"status":"success","data":{"up":[` +
				`{"type":"gauge","help":"target health","unit":""}]}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})
	ctx := context.Background()

	series, _, err := c.Series(ctx, `{__name__="up"}`, 100)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(series) != 2 || series[1]["job"] != "api" {
		t.Errorf("series = %v", series)
	}

	meta, _, err := c.Metadata(ctx, "up")
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if len(meta["up"]) != 1 || meta["up"][0].Type != "gauge" {
		t.Errorf("meta = %v", meta)
	}
}

// A cancelled context must come back as a timeout-classed APIError, not as a
// generic transport failure -- that class is what the error metric counts.
func TestTimeoutIsClassifiable(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := c.Query(ctx, "up", time.Time{}, time.Second)
	if err == nil {
		t.Fatal("a request that outlived its deadline returned no error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if !apiErr.Timeout {
		t.Errorf("Timeout = false for an expired deadline: %v", apiErr)
	}
}

func TestPing(t *testing.T) {
	healthy := true
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/-/healthy" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if !healthy {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping on a healthy server: %v", err)
	}
	healthy = false
	if err := c.Ping(context.Background()); err == nil {
		t.Error("Ping on an unhealthy server returned nil")
	}
}
