package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNoData            = errors.New("prometheus query returned no data")
	ErrMalformedResponse = errors.New("malformed prometheus response")
	ErrNonFiniteValue    = errors.New("prometheus value is not finite")
)

type APIError struct {
	Type    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("prometheus API error (%s): %s", e.Type, e.Message)
}

type Observation struct {
	Value     float64
	Timestamp time.Time
}

type Client struct {
	address    string
	httpClient *http.Client
	timeout    time.Duration
}

func NewClient(address string, timeout time.Duration) (*Client, error) {
	if strings.TrimSpace(address) == "" {
		return nil, fmt.Errorf("prometheus address is required")
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid prometheus address %q", address)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{address: strings.TrimRight(address, "/"), timeout: timeout, httpClient: &http.Client{Timeout: timeout}}, nil
}

type apiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

func (c *Client) Query(ctx context.Context, query string) (Observation, error) {
	if strings.TrimSpace(query) == "" {
		return Observation{}, fmt.Errorf("prometheus query is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u := c.address + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Observation{}, fmt.Errorf("create prometheus request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Observation{}, fmt.Errorf("query prometheus: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Observation{}, fmt.Errorf("prometheus returned HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Observation{}, fmt.Errorf("read prometheus response: %w", err)
	}
	var decoded apiResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Observation{}, fmt.Errorf("%w: %v", ErrMalformedResponse, err)
	}
	if decoded.Status != "success" {
		return Observation{}, &APIError{Type: decoded.ErrorType, Message: decoded.Error}
	}
	if len(decoded.Data.Result) == 0 {
		return Observation{}, ErrNoData
	}
	if len(decoded.Data.Result) != 1 {
		return Observation{}, fmt.Errorf("%w: expected one result, got %d", ErrMalformedResponse, len(decoded.Data.Result))
	}
	value := decoded.Data.Result[0].Value
	if len(value) != 2 {
		return Observation{}, fmt.Errorf("%w: expected timestamp and value", ErrMalformedResponse)
	}
	timestamp, err := rawFloat(value[0])
	if err != nil {
		return Observation{}, fmt.Errorf("%w: invalid timestamp: %v", ErrMalformedResponse, err)
	}
	metric, err := rawFloat(value[1])
	if err != nil {
		return Observation{}, fmt.Errorf("%w: invalid value: %v", ErrMalformedResponse, err)
	}
	if math.IsNaN(metric) || math.IsInf(metric, 0) {
		return Observation{}, ErrNonFiniteValue
	}
	if math.IsNaN(timestamp) || math.IsInf(timestamp, 0) {
		return Observation{}, fmt.Errorf("%w: invalid timestamp", ErrMalformedResponse)
	}
	return Observation{Value: metric, Timestamp: time.Unix(0, int64(timestamp*float64(time.Second)))}, nil
}

func rawFloat(raw json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strconv.ParseFloat(s, 64)
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	return 0, fmt.Errorf("not a numeric JSON value")
}
