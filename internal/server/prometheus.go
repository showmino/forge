package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Prometheus answers instant queries over Prometheus's HTTP API.
type Prometheus struct {
	// URL is the server's address, for example http://prometheus.monitoring.svc:9090.
	URL  string
	HTTP *http.Client
}

// promSample is one series of an instant vector.
type promSample struct {
	labels map[string]string
	value  float64
}

// promMaxBody bounds an answer: a cluster's volumes and nodes are far smaller.
const promMaxBody = 8 << 20

func (p *Prometheus) query(ctx context.Context, q string) ([]promSample, error) {
	hc := p.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.URL, "/")+"/api/v1/query?query="+url.QueryEscape(q), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus answered %s", resp.Status)
	}
	var body struct {
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, promMaxBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("prometheus answer: %w", err)
	}
	if body.Status != "success" {
		return nil, errors.New("prometheus: " + body.Error)
	}
	if body.Data.ResultType != "vector" {
		return nil, fmt.Errorf("prometheus answered a %s, not a vector", body.Data.ResultType)
	}
	out := make([]promSample, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		s, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		out = append(out, promSample{labels: r.Metric, value: v})
	}
	return out, nil
}
