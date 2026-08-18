package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// Endpoint kinds. These are metric dimensions, so they are a closed set by
// construction -- one per API path this package calls, never the metric name
// or the query text.
const (
	EndpointLabelValues = "label_values"
	EndpointMetadata    = "metadata"
	EndpointSeries      = "series"
	EndpointQuery       = "query"
)

// Result types returned by /api/v1/query.
const (
	ResultVector = "vector"
	ResultMatrix = "matrix"
	ResultScalar = "scalar"
	ResultString = "string"
)

// envelope is the wrapper every Prometheus API response arrives in.
type envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Warnings  []string        `json:"warnings"`
}

// Meta is one metadata entry for a metric.
type Meta struct {
	Type string `json:"type"`
	Help string `json:"help"`
	Unit string `json:"unit"`
}

// QueryResult is the payload of an instant query. Result is left raw because
// its shape depends on ResultType; use the As* helpers to decode it.
type QueryResult struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

// Sample is one instant-vector element: a label set and a single value.
type Sample struct {
	Metric map[string]string `json:"metric"`
	Value  SamplePair        `json:"value"`
}

// SampleStream is one range-vector element: a label set and a series of
// values. An instant query returns these when the expression is itself a
// range selector, e.g. `up[5m]`.
type SampleStream struct {
	Metric map[string]string `json:"metric"`
	Values []SamplePair      `json:"values"`
}

// SamplePair is Prometheus's [timestamp, "value"] pair.
//
// The value stays a string on purpose: Prometheus sends it as one so that
// NaN, +Inf and full float64 precision survive the round trip, and every use
// here is display. Parsing it to a float would only lose information.
type SamplePair struct {
	Timestamp float64
	Value     string
}

func (p *SamplePair) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("sample pair is not an array: %w", err)
	}
	if len(raw) != 2 {
		return fmt.Errorf("sample pair has %d elements, want 2", len(raw))
	}
	if err := json.Unmarshal(raw[0], &p.Timestamp); err != nil {
		return fmt.Errorf("sample timestamp: %w", err)
	}
	if err := json.Unmarshal(raw[1], &p.Value); err != nil {
		return fmt.Errorf("sample value: %w", err)
	}
	return nil
}

// Time renders the sample's timestamp as UTC.
func (p SamplePair) Time() time.Time {
	sec, frac := int64(p.Timestamp), p.Timestamp-float64(int64(p.Timestamp))
	return time.Unix(sec, int64(frac*1e9)).UTC()
}

// AsVector decodes an instant-vector result.
func (r *QueryResult) AsVector() ([]Sample, error) {
	var out []Sample
	if err := r.decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// AsMatrix decodes a range-vector result.
func (r *QueryResult) AsMatrix() ([]SampleStream, error) {
	var out []SampleStream
	if err := r.decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// AsScalar decodes a scalar or string result, both of which arrive as a single
// [timestamp, "value"] pair.
func (r *QueryResult) AsScalar() (SamplePair, error) {
	var out SamplePair
	if err := r.decode(&out); err != nil {
		return SamplePair{}, err
	}
	return out, nil
}

func (r *QueryResult) decode(out any) error {
	if len(r.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("decoding %s result: %w", r.ResultType, err)
	}
	return nil
}

// APIError is a refusal from Prometheus rather than a transport failure.
//
// Status, ErrorType and Message are kept separate so the caller can classify
// the failure for metrics while still handing the model Prometheus's own
// wording -- which, for a malformed query, is a position-annotated parse error
// and the most useful thing the tool can say.
type APIError struct {
	Status    int
	Endpoint  string
	ErrorType string
	Message   string

	// Timeout marks a client-side deadline rather than a server verdict.
	Timeout bool
}

func (e *APIError) Error() string {
	switch {
	case e.Message != "" && e.ErrorType != "":
		return fmt.Sprintf("prometheus %s: %s (%s)", e.Endpoint, e.Message, e.ErrorType)
	case e.Message != "":
		return fmt.Sprintf("prometheus %s: %s", e.Endpoint, e.Message)
	case e.Status != 0:
		return fmt.Sprintf("prometheus %s: HTTP %s", e.Endpoint, strconv.Itoa(e.Status))
	default:
		return "prometheus " + e.Endpoint + ": request failed"
	}
}

// transportError wraps a failure that never reached Prometheus's own error
// handling: a refused connection, a DNS miss, or an expired deadline.
func transportError(err error, endpoint, base string) error {
	timeout := errors.Is(err, context.DeadlineExceeded)
	if !timeout {
		var netErr net.Error
		if errors.As(err, &netErr) {
			timeout = netErr.Timeout()
		}
	}

	if timeout {
		return &APIError{
			Endpoint: endpoint,
			Timeout:  true,
			Message:  fmt.Sprintf("timed out reaching prometheus at %s", base),
		}
	}
	return &APIError{
		Endpoint: endpoint,
		Message:  fmt.Sprintf("could not reach prometheus at %s: %v", base, err),
	}
}
