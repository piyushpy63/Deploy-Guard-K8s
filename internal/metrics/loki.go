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

type LokiClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

type LokiResult struct {
	ErrorCount float64
	WarnCount  float64
}

type lokiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func NewLokiClient(baseURL string) *LokiClient {
	return &LokiClient{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (l *LokiClient) QueryCount(logql string) (float64, error) {
	endpoint := fmt.Sprintf("%s/loki/api/v1/query", l.BaseURL)

	params := url.Values{}
	params.Set("query", logql)

	resp, err := l.HTTPClient.Get(endpoint + "?" + params.Encode())
	if err != nil {
		return 0, fmt.Errorf("loki request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("reading response failed: %w", err)
	}

	var result lokiResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("parsing response failed: %w", err)
	}

	if result.Status != "success" {
		return 0, fmt.Errorf("loki returned non-success: %s", result.Status)
	}

	if len(result.Data.Result) == 0 {
		return 0, nil
	}

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

func (l *LokiClient) GetLogMetrics(namespace string) (*LokiResult, error) {
	result := &LokiResult{}
	var err error

	// Count ERROR logs in last 5 minutes
	result.ErrorCount, err = l.QueryCount(
		fmt.Sprintf(`count_over_time({namespace="%s"} |~ "(?i)error" [5m])`, namespace),
	)
	if err != nil {
		return nil, fmt.Errorf("error count query failed: %w", err)
	}

	// Count WARN logs in last 5 minutes
	result.WarnCount, err = l.QueryCount(
		fmt.Sprintf(`count_over_time({namespace="%s"} |~ "(?i)warn" [5m])`, namespace),
	)
	if err != nil {
		return nil, fmt.Errorf("warn count query failed: %w", err)
	}

	return result, nil
}
