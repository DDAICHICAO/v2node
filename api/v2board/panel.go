package panel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/go-resty/resty/v2"
	"github.com/wyx2685/v2node/common/instance"
	"github.com/wyx2685/v2node/common/publicip"
	selfversion "github.com/wyx2685/v2node/common/version"
	"github.com/wyx2685/v2node/conf"
)

// Panel is the interface for different panel's api.

var deviceLimitCapabilities = []string{
	"device_uuid_users",
	"uid_traffic_aggregate",
	"device_traffic_report",
	"device_alive_report",
	"device_limit_by_uuid",
	"stream_unlock_test",
	"user_delta_sync",
	"user_sync_wakeup_v1",
	"uuid_ip_fanout_guard",
	"uuid_ip_fanout_reservation_v1",
	"device_limit_event_report",
	"managed_tls_certificate_v1",
	managedTLSAutoCredentialCapability,
	"managed_tls_domain_migration_v1",
}

type Client struct {
	client                       *resty.Client
	managedTLSClient             *resty.Client
	APIHost                      string
	Token                        string
	AppTransportTokenSecret      string
	NodeId                       int
	nodeInfoMu                   sync.Mutex
	nodeEtag                     string
	nodeConfigVersion            string
	userEtag                     string
	userSyncMu                   sync.RWMutex
	userSyncSeq                  int64
	responseBodyHash             string
	instanceID                   string
	tlsCertificateToken          string
	tlsCertificateTokenExplicit  bool
	managedTLSCredentialMu       sync.RWMutex
	managedTLSCredentialStore    *managedTLSCredentialStore
	managedTLSCredentialRequired atomic.Bool
	managedTLSNow                func() time.Time
	managedTLSNonce              func() (string, error)
	UserList                     *UserListBody
	AliveMap                     *AliveMap
}

func New(c *conf.NodeConfig) (*Client, error) {
	client := resty.New()
	retryCount := conf.DefaultNodeRetryCount
	if c.RetryCount != nil {
		retryCount = *c.RetryCount
	}
	client.SetRetryCount(retryCount)
	client.SetHeader("User-Agent", fmt.Sprintf("v2node go-resty/%s (https://github.com/go-resty/resty)", resty.Version))
	if c.Timeout > 0 {
		client.SetTimeout(time.Duration(c.Timeout) * time.Second)
	} else {
		client.SetTimeout(time.Duration(conf.DefaultNodeTimeout) * time.Second)
	}
	client.OnError(func(req *resty.Request, err error) {
		var v *resty.ResponseError
		if errors.As(err, &v) {
			// v.Response contains the last response from the server
			// v.Err contains the original error
			logrus.Error(v.Err)
		}
	})
	client.SetBaseURL(c.APIHost)
	managedTLSClient := resty.New()
	managedTLSClient.SetRetryCount(retryCount)
	managedTLSClient.SetHeader("User-Agent", fmt.Sprintf("v2node go-resty/%s (https://github.com/go-resty/resty)", resty.Version))
	if c.Timeout > 0 {
		managedTLSClient.SetTimeout(time.Duration(c.Timeout) * time.Second)
	} else {
		managedTLSClient.SetTimeout(time.Duration(conf.DefaultNodeTimeout) * time.Second)
	}
	managedTLSClient.SetBaseURL(c.APIHost)
	// set params
	queryParams := map[string]string{
		"node_type": "v2node",
		"node_id":   strconv.Itoa(c.NodeID),
		"token":     c.Key,
	}
	if currentVersion := selfversion.Current(); currentVersion != "" {
		queryParams["current_version"] = currentVersion
	}
	resolvedInstanceID := instance.ResolveID(c.APIHost, c.NodeID)
	queryParams["instance_id"] = resolvedInstanceID
	queryParams["capabilities"] = strings.Join(deviceLimitCapabilities, ",")
	configuredMachineIP := publicip.Normalize(c.MachineIP)
	if configuredMachineIP != "" {
		queryParams["machine_ip"] = configuredMachineIP
	}
	client.SetQueryParams(queryParams)
	client.OnBeforeRequest(func(_ *resty.Client, req *resty.Request) error {
		if configuredMachineIP != "" || req.QueryParam.Get("machine_ip") != "" {
			return nil
		}
		ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
		defer cancel()
		if machineIP := publicip.Detect(ctx); machineIP != "" {
			req.SetQueryParam("machine_ip", machineIP)
		}
		return nil
	})
	tlsCertificateToken := strings.TrimSpace(c.TLSCertificateToken)
	credentialStore := newManagedTLSCredentialStore(managedTLSCredentialBaseDir, c.APIHost, c.NodeID)
	if tlsCertificateToken == "" {
		if credential, err := credentialStore.Load(); err == nil {
			tlsCertificateToken = credential.Token
		}
	}
	return &Client{
		client:                      client,
		managedTLSClient:            managedTLSClient,
		Token:                       c.Key,
		AppTransportTokenSecret:     c.AppTransportTokenSecret,
		APIHost:                     c.APIHost,
		NodeId:                      c.NodeID,
		instanceID:                  resolvedInstanceID,
		tlsCertificateToken:         tlsCertificateToken,
		tlsCertificateTokenExplicit: strings.TrimSpace(c.TLSCertificateToken) != "",
		managedTLSCredentialStore:   credentialStore,
		UserList:                    &UserListBody{},
		AliveMap:                    &AliveMap{},
	}, nil
}

func (c *Client) InstanceID() string {
	if c == nil {
		return ""
	}
	return c.instanceID
}

func (c *Client) NodeInfoVersion() string {
	if c == nil {
		return ""
	}
	c.nodeInfoMu.Lock()
	defer c.nodeInfoMu.Unlock()
	return c.nodeConfigVersion
}
