package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type PrometheusClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

type QueryResult struct {
	ErrorRate   float64
	P95Latency  float64
	PodRestarts float64
	OOMKills    float64
}

type prometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func NewPrometheusClient(baseURL string) *PrometheusClient {
	return &PrometheusClient{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (p *PrometheusClient) Query(promql string) (float64, error) {
	endpoint := fmt.Sprintf("%s/api/v1/query", p.BaseURL)

	params := url.Values{}
	params.Set("query", promql)

	resp, err := p.HTTPClient.Get(endpoint + "?" + params.Encode())
	if err != nil {
		return 0, fmt.Errorf("prometheus request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("reading response failed: %w", err)
	}

	var result prometheusResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("parsing response failed: %w", err)
	}

	if result.Status != "success" {
		return 0, fmt.Errorf("prometheus returned non-success status: %s", result.Status)
	}

	if len(result.Data.Result) == 0 {
		return 0, nil // no data yet, return 0
	}

	// value is [timestamp, "value_string"]
	valueStr, ok := result.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected value format")
	}

	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing float failed: %w", err)
	}

	return value, nil
}

func (p *PrometheusClient) GetMetrics(namespace string) (*QueryResult, error) {
	result := &QueryResult{}
	var err error

	// Error rate — requests per second returning 5xx
	result.ErrorRate, err = p.Query(
		fmt.Sprintf(`sum(rate(http_requests_total{namespace="%s",status=~"5.."}[5m]))`, namespace),
	)
	if err != nil {
		return nil, fmt.Errorf("error rate query failed: %w", err)
	}

	// P95 latency in milliseconds
	result.P95Latency, err = p.Query(
		fmt.Sprintf(`histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{namespace="%s"}[5m])) by (le)) * 1000`, namespace),
	)
	if err != nil {
		return nil, fmt.Errorf("p95 latency query failed: %w", err)
	}

	// Pod restarts
	result.PodRestarts, err = p.Query(
		fmt.Sprintf(`sum(kube_pod_container_status_restarts_total{namespace="%s"})`, namespace),
	)
	if err != nil {
		return nil, fmt.Errorf("pod restarts query failed: %w", err)
	}

	// OOM kills
	result.OOMKills, err = p.Query(
		fmt.Sprintf(`sum(kube_pod_container_status_last_terminated_reason{namespace="%s",reason="OOMKilled"})`, namespace),
	)
	if err != nil {
		return nil, fmt.Errorf("oom kills query failed: %w", err)
	}

	return result, nil
}
