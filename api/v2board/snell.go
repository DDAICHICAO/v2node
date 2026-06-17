package panel

import (
	"context"
	"fmt"

	"encoding/json/v2"
)

type ManagedSnellResponse struct {
	Data ManagedSnellState `json:"data"`
}

type ManagedSnellState struct {
	GeneratedAt int64              `json:"generated_at"`
	Snell       []ManagedSnellNode `json:"snell"`
}

type ManagedSnellNode struct {
	ID          int                      `json:"id"`
	Name        string                   `json:"name"`
	Host        string                   `json:"host"`
	ListenIP    string                   `json:"listen_ip"`
	Version     int                      `json:"version"`
	Obfs        string                   `json:"obfs"`
	ObfsHost    string                   `json:"obfs_host"`
	Credentials []ManagedSnellCredential `json:"credentials"`
}

type ManagedSnellCredential struct {
	UserID int    `json:"user_id"`
	Port   int    `json:"port"`
	PSK    string `json:"psk"`
	Status string `json:"status"`
}

type ManagedSnellTrafficUser struct {
	UserID   int   `json:"user_id"`
	Port     int   `json:"port"`
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
}

type managedSnellTrafficRequest struct {
	SnellID int                       `json:"snell_id"`
	Mode    string                    `json:"mode,omitempty"`
	Data    []ManagedSnellTrafficUser `json:"data"`
}

type managedSnellTrafficResponse struct {
	Data struct {
		AcceptedUsers int    `json:"accepted_users"`
		RejectedUsers int    `json:"rejected_users"`
		AcceptedBytes int64  `json:"accepted_bytes"`
		Message       string `json:"message"`
	} `json:"data"`
	Message string `json:"message"`
}

func (c *Client) GetManagedSnell(ctx context.Context) (*ManagedSnellState, error) {
	const path = "/api/v2/server/snell/config"
	r, err := c.client.R().
		SetContext(ctx).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("received nil managed snell response")
	}
	if r.StatusCode() >= 400 {
		return nil, fmt.Errorf("managed snell request failed: status %d", r.StatusCode())
	}

	var response ManagedSnellResponse
	if err := json.Unmarshal(r.Body(), &response); err != nil {
		return nil, fmt.Errorf("decode managed snell error: %w", err)
	}
	return &response.Data, nil
}

func (c *Client) ReportManagedSnellStatus(ctx context.Context, payload any) error {
	const path = "/api/v2/server/snell/statuses"
	r, err := c.client.R().
		SetContext(ctx).
		SetBody(payload).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("received nil managed snell status response")
	}
	if r.StatusCode() >= 400 {
		return fmt.Errorf("managed snell status report failed: status %d", r.StatusCode())
	}
	return nil
}

func (c *Client) ReportManagedSnellTraffic(ctx context.Context, snellID int, data []ManagedSnellTrafficUser) error {
	path := fmt.Sprintf("/api/v2/server/snell/%d/traffic", snellID)
	r, err := c.client.R().
		SetContext(ctx).
		SetBody(managedSnellTrafficRequest{
			SnellID: snellID,
			Mode:    "delta",
			Data:    data,
		}).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("received nil managed snell traffic response")
	}
	if r.StatusCode() >= 400 {
		return fmt.Errorf("managed snell traffic report failed: status %d", r.StatusCode())
	}
	var response managedSnellTrafficResponse
	if err := json.Unmarshal(r.Body(), &response); err != nil {
		return fmt.Errorf("decode managed snell traffic response: %w", err)
	}
	if response.Message != "" {
		return fmt.Errorf("managed snell traffic report rejected: %s", response.Message)
	}
	if response.Data.Message != "" {
		return fmt.Errorf("managed snell traffic report rejected: %s", response.Data.Message)
	}
	if response.Data.RejectedUsers > 0 {
		return fmt.Errorf("managed snell traffic report rejected %d users", response.Data.RejectedUsers)
	}
	if len(data) > 0 && response.Data.AcceptedUsers == 0 {
		return fmt.Errorf("managed snell traffic report accepted no users")
	}
	return nil
}
