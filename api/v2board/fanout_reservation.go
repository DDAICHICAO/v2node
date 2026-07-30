package panel

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
)

var (
	ErrFanoutReservationFallback  = errors.New("fanout reservation fallback")
	ErrFanoutReservationProtocol  = errors.New("fanout reservation protocol error")
	ErrFanoutReservationTemporary = errors.New("fanout reservation temporarily unavailable")
)

type FanoutReservationError struct {
	Kind       error
	StatusCode int
	Message    string
}

func (e *FanoutReservationError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *FanoutReservationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Kind
}

func FanoutReservationStatus(err error) int {
	var target *FanoutReservationError
	if errors.As(err, &target) {
		return target.StatusCode
	}
	return 0
}

type FanoutReservationKind string

const (
	FanoutReservationAllow  FanoutReservationKind = "allow"
	FanoutReservationReject FanoutReservationKind = "reject"
)

type FanoutReservationRequest struct {
	UserID     int    `json:"user_id"`
	UUID       string `json:"uuid"`
	IP         string `json:"ip"`
	RequestID  string `json:"request_id"`
	ObservedAt int64  `json:"observed_at"`
}

type FanoutReservationData struct {
	Kind          FanoutReservationKind `json:"-"`
	Decision      string                `json:"decision"`
	Scope         string                `json:"scope"`
	Existing      bool                  `json:"existing"`
	UniqueIPCount int                   `json:"unique_ip_count"`
	Threshold     int                   `json:"threshold"`
	WindowSeconds int                   `json:"window_seconds"`
	ExpiresAt     int64                 `json:"expires_at"`
	Revision      int64                 `json:"revision"`
}

func (c *Client) ReserveUUIDIPFanout(
	ctx context.Context,
	request FanoutReservationRequest,
) (*FanoutReservationData, error) {
	const path = "/api/v2/server/uuid-ip-fanout/reserve"
	r, err := c.client.R().
		SetContext(ctx).
		SetBody(request).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return nil, &FanoutReservationError{
			Kind:    ErrFanoutReservationTemporary,
			Message: "fanout reservation request failed",
		}
	}
	if r == nil {
		return nil, &FanoutReservationError{
			Kind:    ErrFanoutReservationTemporary,
			Message: "received nil fanout reservation response",
		}
	}

	status := r.StatusCode()
	message := bodySnippet(r.Body())
	switch {
	case status == 404 || status == 409:
		return nil, &FanoutReservationError{
			Kind: ErrFanoutReservationFallback, StatusCode: status, Message: message,
		}
	case status == 400 || status == 401 || status == 403 || status == 422:
		return nil, &FanoutReservationError{
			Kind: ErrFanoutReservationProtocol, StatusCode: status, Message: message,
		}
	case status == 429 || status >= 500:
		return nil, &FanoutReservationError{
			Kind: ErrFanoutReservationTemporary, StatusCode: status, Message: message,
		}
	case status >= 400:
		return nil, &FanoutReservationError{
			Kind: ErrFanoutReservationProtocol, StatusCode: status, Message: message,
		}
	}

	var body struct {
		Data FanoutReservationData `json:"data"`
	}
	if err := json.Unmarshal(r.Body(), &body); err != nil {
		return nil, &FanoutReservationError{
			Kind: ErrFanoutReservationProtocol, StatusCode: status, Message: err.Error(),
		}
	}
	switch strings.ToLower(strings.TrimSpace(body.Data.Decision)) {
	case "allow":
		body.Data.Kind = FanoutReservationAllow
	case "reject":
		body.Data.Kind = FanoutReservationReject
	default:
		return nil, &FanoutReservationError{
			Kind:       ErrFanoutReservationProtocol,
			StatusCode: status,
			Message:    "invalid fanout reservation decision",
		}
	}
	return &body.Data, nil
}
