package panel

import (
	"context"
	"fmt"

	"encoding/json/v2"
)

type StreamUnlockTask struct {
	Enabled  bool     `json:"enabled"`
	TaskID   string   `json:"task_id"`
	Services []string `json:"services"`
	Timeout  int      `json:"timeout"`
}

type StreamUnlockResult struct {
	Service   string `json:"service"`
	Status    string `json:"status"`
	Region    string `json:"region"`
	Title     string `json:"title"`
	Message   string `json:"message"`
	LatencyMs int64  `json:"latency_ms"`
}

type StreamUnlockReport struct {
	TaskID     string               `json:"task_id"`
	Status     string               `json:"status"`
	Message    string               `json:"message"`
	Results    []StreamUnlockResult `json:"results"`
	DurationMs int64                `json:"duration_ms"`
}

func (c *Client) GetStreamUnlockTask(ctx context.Context) (*StreamUnlockTask, error) {
	const path = "/api/v2/server/stream-unlock"
	r, err := c.client.R().
		SetContext(ctx).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("received nil response")
	}
	if r.StatusCode() >= 400 {
		return nil, fmt.Errorf("stream unlock task request failed: status %d", r.StatusCode())
	}

	var body struct {
		Data *StreamUnlockTask `json:"data"`
	}
	if err := json.Unmarshal(r.Body(), &body); err != nil {
		return nil, fmt.Errorf("decode stream unlock task error: %w", err)
	}
	return body.Data, nil
}

func (c *Client) ReportStreamUnlockStatus(ctx context.Context, report StreamUnlockReport) error {
	const path = "/api/v2/server/stream-unlock/report"
	r, err := c.client.R().
		SetContext(ctx).
		SetBody(report).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("received nil response")
	}
	if r.StatusCode() >= 400 {
		return fmt.Errorf("stream unlock report failed: status %d", r.StatusCode())
	}
	return nil
}
