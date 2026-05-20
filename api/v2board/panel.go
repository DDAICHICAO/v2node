package panel

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/go-resty/resty/v2"
	"github.com/wyx2685/v2node/common/instance"
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
}

type Client struct {
	client                  *resty.Client
	APIHost                 string
	Token                   string
	AppTransportTokenSecret string
	NodeId                  int
	nodeEtag                string
	userEtag                string
	responseBodyHash        string
	UserList                *UserListBody
	AliveMap                *AliveMap
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
	// set params
	queryParams := map[string]string{
		"node_type": "v2node",
		"node_id":   strconv.Itoa(c.NodeID),
		"token":     c.Key,
	}
	if currentVersion := selfversion.Current(); currentVersion != "" {
		queryParams["current_version"] = currentVersion
	}
	queryParams["instance_id"] = instance.ResolveID(c.APIHost, c.NodeID)
	queryParams["capabilities"] = strings.Join(deviceLimitCapabilities, ",")
	client.SetQueryParams(queryParams)
	return &Client{
		client:                  client,
		Token:                   c.Key,
		AppTransportTokenSecret: c.AppTransportTokenSecret,
		APIHost:                 c.APIHost,
		NodeId:                  c.NodeID,
		UserList:                &UserListBody{},
		AliveMap:                &AliveMap{},
	}, nil
}
