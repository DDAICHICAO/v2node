package panel

import (
	"context"
	"fmt"

	"encoding/json/v2"
)

type UpdateTask struct {
	Enabled     bool             `json:"enabled"`
	TaskID      string           `json:"task_id"`
	Type        string           `json:"type"`
	Version     string           `json:"version"`
	DownloadURL string           `json:"download_url"`
	SHA256      string           `json:"sha256"`
	AccessAudit *AccessAuditTask `json:"access_audit,omitempty"`
}

type AccessAuditTask struct {
	Enabled       bool   `json:"enabled"`
	Endpoint      string `json:"endpoint"`
	Token         string `json:"token"`
	BatchSize     int    `json:"batch_size"`
	MaxQueueSize  int    `json:"max_queue_size"`
	FlushInterval string `json:"flush_interval"`
	Timeout       string `json:"timeout"`
	SNTPAccess    *bool  `json:"sntp_access,omitempty"`
}

type UpdateReport struct {
	TaskID         string `json:"task_id"`
	Version        string `json:"version"`
	CurrentVersion string `json:"current_version"`
	Status         string `json:"status"`
	Message        string `json:"message"`
}

func (c *Client) GetUpdateTask(ctx context.Context) (*UpdateTask, error) {
	const path = "/api/v2/server/update"
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
		return nil, fmt.Errorf("update task request failed: status %d", r.StatusCode())
	}

	var body struct {
		Data *UpdateTask `json:"data"`
	}
	if err := json.Unmarshal(r.Body(), &body); err != nil {
		return nil, fmt.Errorf("decode update task error: %w", err)
	}
	return body.Data, nil
}

func (c *Client) ReportUpdateStatus(ctx context.Context, report UpdateReport) error {
	const path = "/api/v2/server/update/report"
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
		return fmt.Errorf("update report failed: status %d", r.StatusCode())
	}
	return nil
}
