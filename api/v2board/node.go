package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	stdnet "net"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"encoding/json"
)

// Security type
const (
	None    = 0
	Tls     = 1
	Reality = 2
)

type NodeInfo struct {
	Id           int
	Type         string
	Security     int
	PushInterval time.Duration
	PullInterval time.Duration
	Tag          string
	Common       *CommonNode
}

type CommonNode struct {
	Protocol   string      `json:"protocol"`
	ListenIP   string      `json:"listen_ip"`
	ServerPort int         `json:"server_port"`
	Routes     []Route     `json:"routes"`
	BaseConfig *BaseConfig `json:"base_config"`
	//vless vmess trojan
	Tls                int         `json:"tls"`
	TlsSettings        TlsSettings `json:"tls_settings"`
	CertInfo           *CertInfo
	Network            string          `json:"network"`
	NetworkSettings    json.RawMessage `json:"network_settings"`
	Encryption         string          `json:"encryption"`
	EncryptionSettings EncSettings     `json:"encryption_settings"`
	ServerName         string          `json:"server_name"`
	Flow               string          `json:"flow"`
	//shadowsocks
	Cipher    string `json:"cipher"`
	ServerKey string `json:"server_key"`
	//tuic
	CongestionControl string `json:"congestion_control"`
	ZeroRTTHandshake  bool   `json:"zero_rtt_handshake"`
	//anytls
	PaddingScheme []string `json:"padding_scheme,omitempty"`
	//hysteria hysteria2
	UpMbps                  int    `json:"up_mbps"`
	DownMbps                int    `json:"down_mbps"`
	Obfs                    string `json:"obfs"`
	ObfsPassword            string `json:"obfs_password"`
	Ignore_Client_Bandwidth bool   `json:"ignore_client_bandwidth"`
}

type Route struct {
	Id          int      `json:"id"`
	Match       []string `json:"match"`
	Action      string   `json:"action"`
	ActionValue *string  `json:"action_value"`
}

type BaseConfig struct {
	PushInterval               any                   `json:"push_interval"`
	PullInterval               any                   `json:"pull_interval"`
	DeviceOnlineMinTraffic     int                   `json:"device_online_min_traffic"`
	NodeReportMinTraffic       int                   `json:"node_report_min_traffic"`
	DeviceTrafficReport        bool                  `json:"device_traffic_report"`
	DeviceAliveReport          bool                  `json:"device_alive_report"`
	DeviceLimitByUUID          bool                  `json:"device_limit_by_uuid"`
	DeviceLimitEnforcementMode string                `json:"device_limit_enforcement_mode"`
	UUIDIPFanoutGuard          *UUIDIPFanoutConfig   `json:"uuid_ip_fanout_guard"`
	UserSyncWakeup             *UserSyncWakeupConfig `json:"user_sync_wakeup"`
	// Optional seconds between SNTP Eclipse online refresh marks.
	SntpEclipseOnlineRefresh any `json:"sntp_eclipse_online_refresh"`
}

type UserSyncWakeupConfig struct {
	Enabled          bool   `json:"enabled"`
	Path             string `json:"path"`
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
	FallbackPollMS   int    `json:"fallback_poll_ms"`
	MergeMS          int    `json:"merge_ms"`
}

func (c *UserSyncWakeupConfig) Normalize() {
	if c == nil {
		return
	}
	if !strings.HasPrefix(c.Path, "/") {
		c.Path = "/api/v2/server/user-sync/wakeup"
	}
	c.HeartbeatSeconds = clampInt(c.HeartbeatSeconds, 10, 60, 25)
	c.FallbackPollMS = clampInt(c.FallbackPollMS, 1000, 10000, 2000)
	c.MergeMS = clampInt(c.MergeMS, 100, 1000, 250)
}

func clampInt(value, minValue, maxValue, defaultValue int) int {
	if value == 0 {
		return defaultValue
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

type UUIDIPFanoutConfig struct {
	Enabled              bool                           `json:"enabled"`
	Mode                 string                         `json:"mode"`
	WindowSeconds        int                            `json:"window_seconds"`
	MaxUniqueIPs         int                            `json:"max_unique_ips"`
	EventCooldownSeconds int                            `json:"event_cooldown_seconds"`
	WhitelistCIDRs       []string                       `json:"whitelist_cidrs"`
	Strategy             string                         `json:"strategy"`
	Reservation          *UUIDIPFanoutReservationConfig `json:"reservation"`
}

type UUIDIPFanoutReservationConfig struct {
	Enabled     bool   `json:"enabled"`
	TimeoutMS   int    `json:"timeout_ms"`
	FailureMode string `json:"failure_mode"`
}

type TlsSettings struct {
	ServerName         string   `json:"server_name"`
	ServerNames        []string `json:"server_names"`
	Dest               string   `json:"dest"`
	ServerPort         string   `json:"server_port"`
	ShortId            string   `json:"short_id"`
	ShortIds           []string `json:"short_ids"`
	PrivateKey         string   `json:"private_key"`
	Mldsa65Seed        string   `json:"mldsa65Seed"`
	Xver               uint64   `json:"xver,string"`
	CertMode           string   `json:"cert_mode"`
	CertFile           string   `json:"cert_file"`
	KeyFile            string   `json:"key_file"`
	Provider           string   `json:"provider"`
	DNSEnv             string   `json:"dns_env"`
	RejectUnknownSni   string   `json:"reject_unknown_sni"`
	CertificateScopeID uint64   `json:"certificate_scope_id"`
	FrontMode          string   `json:"front_mode"`
	ECH                string   `json:"ech"`
	ECHServerName      string   `json:"ech_server_name"`
	ECHKey             string   `json:"ech_key"`
	ECHConfig          string   `json:"ech_config"`
}

func (t *TlsSettings) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" || text == "[]" {
		*t = TlsSettings{}
		return nil
	}
	type alias TlsSettings
	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*t = TlsSettings(value)
	return nil
}

type CertInfo struct {
	CertMode         string
	CertFile         string
	KeyFile          string
	Email            string
	CertDomain       string
	DNSEnv           map[string]string
	Provider         string
	RejectUnknownSni bool
}

type EncSettings struct {
	NativeMaxHandshakes      int    `json:"native_max_handshakes"`
	NativeMaxConnections     int    `json:"native_max_connections"`
	NativeMaxUserConnections int    `json:"native_max_user_connections"`
	NativeReplayCapacity     int    `json:"native_replay_capacity"`
	NativePreauthRatePerIP   int    `json:"native_preauth_rate_per_ip"`
	NativePreauthBurstPerIP  int    `json:"native_preauth_burst_per_ip"`
	NativeReplayPerUser      int    `json:"native_replay_per_user"`
	NativeReplayPerIP        int    `json:"native_replay_per_ip"`
	Mode                     string `json:"mode"`
	Ticket                   string `json:"ticket"`
	ServerPadding            string `json:"server_padding"`
	PrivateKey               string `json:"private_key"`
	PublicKey                string `json:"public_key"`
}

func (e *EncSettings) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" || text == "[]" {
		*e = EncSettings{}
		return nil
	}
	type alias EncSettings
	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*e = EncSettings(value)
	return nil
}

// ErrNodeNotFound is only returned for an explicit panel business response,
// never for a generic HTTP 404, network failure, or authorization rejection.
var ErrNodeNotFound = errors.New("panel node no longer exists")

func (c *Client) GetNodeInfo(ctx context.Context) (node *NodeInfo, err error) {
	if c.managedTLSCredentialRequired.Load() {
		_ = c.EnsureManagedTLSCredential(ctx, false)
	}
	c.nodeInfoMu.Lock()
	defer c.nodeInfoMu.Unlock()

	const path = "/api/v2/server/config"
	r, err := c.client.
		R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.nodeEtag).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("received nil response")
	}
	if r.RawBody() != nil {
		defer r.RawBody().Close()
	}

	if r.StatusCode() == 304 {
		return nil, nil
	}
	if r.StatusCode() == 401 || r.StatusCode() == 403 {
		return nil, &ValidationError{Operation: "get node info", StatusCode: r.StatusCode(), Message: "node authorization rejected"}
	}
	var failure struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if r.StatusCode() == 200 && json.Unmarshal(r.Body(), &failure) == nil && failure.Status == "fail" {
		switch failure.Message {
		case "server is not exist":
			return nil, ErrNodeNotFound
		case "token is null", "token is error":
			return nil, &ValidationError{Operation: "get node info", StatusCode: r.StatusCode(), Message: "node authorization rejected"}
		default:
			return nil, errors.New("panel rejected node configuration request")
		}
	}
	if r.StatusCode() >= 400 {
		return nil, fmt.Errorf("get node info http status %d: %s", r.StatusCode(), bodySnippet(r.Body()))
	}
	hash := sha256.Sum256(r.Body())
	newBodyHash := hex.EncodeToString(hash[:])
	if c.responseBodyHash == newBodyHash {
		c.nodeEtag = r.Header().Get("ETag")
		return nil, nil
	}
	node = &NodeInfo{
		Id: c.NodeId,
	}
	// parse protocol params
	cm := &CommonNode{}
	err = json.Unmarshal(r.Body(), cm)
	if err != nil {
		return nil, fmt.Errorf("decode node params error: %s; body=%s", err, bodySnippet(r.Body()))
	}
	if err := ValidateIngressMode(cm); err != nil {
		return nil, fmt.Errorf("invalid ingress mode: %w", err)
	}
	managedTLSRequired := cm.TlsSettings.CertMode == "managed"
	c.managedTLSCredentialRequired.Store(managedTLSRequired)
	if managedTLSRequired {
		// Credential bootstrap is best effort. The managed TLS manager keeps the
		// node blocked until a credential becomes available and retries on later
		// config pulls without taking unrelated instances down.
		_ = c.EnsureManagedTLSCredential(ctx, false)
	}
	if cm.BaseConfig != nil && cm.BaseConfig.UserSyncWakeup != nil {
		cm.BaseConfig.UserSyncWakeup.Normalize()
	}
	switch cm.Protocol {
	case "vmess", "trojan", "hysteria2", "tuic", "anytls", "vless", "sntp-eclipse", "mieru":
		node.Type = cm.Protocol
		node.Security = cm.Tls
	case "shadowsocks":
		node.Type = cm.Protocol
		node.Security = 0
	default:
		return nil, fmt.Errorf("unsupport protocol: %s", cm.Protocol)
	}
	node.Tag = fmt.Sprintf("[%s]-%s:%d", c.APIHost, node.Type, node.Id)
	cf := cm.TlsSettings.CertFile
	kf := cm.TlsSettings.KeyFile
	if cm.TlsSettings.CertMode == "managed" {
		cf, kf = managedTLSCertificatePaths(c.NodeId)
	}
	if cf == "" {
		cf = filepath.Join("/etc/v2node/", cm.Protocol+strconv.Itoa(c.NodeId)+".cer")
	}
	if kf == "" {
		kf = filepath.Join("/etc/v2node/", cm.Protocol+strconv.Itoa(c.NodeId)+".key")
	}
	cm.CertInfo = &CertInfo{
		CertMode:         cm.TlsSettings.CertMode,
		CertFile:         cf,
		KeyFile:          kf,
		Email:            "node@v2board.com",
		CertDomain:       cm.TlsSettings.PrimaryServerName(),
		DNSEnv:           make(map[string]string),
		Provider:         cm.TlsSettings.Provider,
		RejectUnknownSni: cm.TlsSettings.RejectUnknownSni == "1",
	}
	if cm.CertInfo.CertMode == "dns" && cm.TlsSettings.DNSEnv != "" {
		envs := strings.Split(cm.TlsSettings.DNSEnv, ",")
		for _, env := range envs {
			kv := strings.SplitN(env, "=", 2)
			if len(kv) == 2 {
				cm.CertInfo.DNSEnv[kv[0]] = kv[1]
			}
		}
	}

	// set interval
	node.PushInterval = intervalToTime(cm.BaseConfig.PushInterval)
	node.PullInterval = intervalToTime(cm.BaseConfig.PullInterval)

	node.Common = cm
	c.responseBodyHash = newBodyHash
	c.nodeEtag = r.Header().Get("ETag")
	c.nodeConfigVersion = newBodyHash

	return node, nil
}

func managedTLSCertificatePaths(nodeID int) (string, string) {
	base := filepath.Join("/etc/v2node/certificates", strconv.Itoa(nodeID), "current")
	return filepath.Join(base, "fullchain.pem"), filepath.Join(base, "private.key")
}

func bodySnippet(body []byte) string {
	value := strings.TrimSpace(string(body))
	if len(value) > 300 {
		value = value[:300] + "..."
	}
	return value
}

func intervalToTime(i interface{}) time.Duration {
	switch reflect.TypeOf(i).Kind() {
	case reflect.Int:
		return time.Duration(i.(int)) * time.Second
	case reflect.String:
		i, _ := strconv.Atoi(i.(string))
		return time.Duration(i) * time.Second
	case reflect.Float64:
		return time.Duration(i.(float64)) * time.Second
	default:
		return time.Duration(reflect.ValueOf(i).Int()) * time.Second
	}
}

func (t TlsSettings) EffectiveServerNames() []string {
	if len(t.ServerNames) > 0 {
		return t.ServerNames
	}
	if t.ServerName == "" {
		return nil
	}
	return []string{t.ServerName}
}

func (t TlsSettings) EffectiveShortIds() []string {
	if len(t.ShortIds) > 0 {
		return t.ShortIds
	}
	if t.ShortId == "" {
		return nil
	}
	return []string{t.ShortId}
}

func (t TlsSettings) PrimaryServerName() string {
	serverNames := t.EffectiveServerNames()
	if len(serverNames) == 0 {
		return ""
	}
	return serverNames[0]
}

// ValidateIngressMode rejects an invalid SNI-fronted node before the runtime
// can attempt to create a listener. Direct mode remains compatible with the
// existing V2Node combinations, and Titan is kept on its own protocol path.
func ValidateIngressMode(common *CommonNode) error {
	if common == nil {
		return errors.New("missing node configuration")
	}

	frontMode := strings.ToLower(strings.TrimSpace(common.TlsSettings.FrontMode))
	if frontMode == "" || frontMode == "direct" {
		if common.Protocol == "sntp-eclipse" && frontMode == "direct" {
			return nil
		}
		return nil
	}
	if frontMode != "sni" {
		return fmt.Errorf("unsupported front_mode: %s", frontMode)
	}
	if common.Protocol == "sntp-eclipse" {
		return errors.New("Titan does not support SNI front mode")
	}
	switch common.Protocol {
	case "trojan", "anytls", "vless":
	default:
		return fmt.Errorf("SNI front mode does not support protocol: %s", common.Protocol)
	}
	if common.Tls != Tls {
		return errors.New("SNI front mode requires ordinary TLS")
	}
	if strings.ToLower(strings.TrimSpace(common.Network)) != "tcp" {
		return errors.New("SNI front mode requires TCP transport")
	}
	if common.ServerPort < 1 || common.ServerPort > 65535 {
		return errors.New("SNI front mode requires server_port in range 1..65535")
	}
	if !validIngressDNSName(common.TlsSettings.ServerName) {
		return errors.New("SNI front mode requires a valid DNS SNI")
	}
	certMode := strings.ToLower(strings.TrimSpace(common.TlsSettings.CertMode))
	switch certMode {
	case "self", "http", "dns", "file", "managed":
	default:
		return errors.New("SNI front mode requires a usable certificate mode")
	}
	if common.TlsSettings.ECH != "" || common.TlsSettings.ECHServerName != "" ||
		common.TlsSettings.ECHKey != "" || common.TlsSettings.ECHConfig != "" {
		return errors.New("SNI front mode does not support ECH")
	}
	if strings.TrimSpace(common.Flow) != "" {
		return errors.New("SNI front mode does not support flow")
	}
	return validateIngressNetworkSettings(common.NetworkSettings)
}

func validateIngressNetworkSettings(raw json.RawMessage) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" {
		return nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("SNI front mode network_settings must be an object: %w", err)
	}
	value, ok := settings["acceptProxyProtocol"]
	if !ok {
		return nil
	}
	text := strings.TrimSpace(string(value))
	if text != "true" && text != "false" {
		return errors.New("acceptProxyProtocol must be boolean")
	}
	return nil
}

func validIngressDNSName(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 253 || stdnet.ParseIP(value) != nil {
		return false
	}
	labels := strings.Split(value, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range []byte(label) {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
				(char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}
