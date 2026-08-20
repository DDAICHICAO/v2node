package dispatcher

import (
	"context"
	"errors"
	"sync"

	"github.com/xtls/xray-core/common"
	xnet "github.com/xtls/xray-core/common/net"
	featuredns "github.com/xtls/xray-core/features/dns"
)

type RuntimeDNSClient struct {
	mu      sync.RWMutex
	manager *RouteRuntimeManager
	closed  bool
}

func init() {
	common.Must(common.RegisterConfig((*SessionConfig)(nil), func(context.Context, interface{}) (interface{}, error) {
		return NewRuntimeDNSClient(), nil
	}))
}

func RuntimeDNSFeatureConfig() *SessionConfig {
	return &SessionConfig{}
}

func NewRuntimeDNSClient() *RuntimeDNSClient {
	return &RuntimeDNSClient{}
}

func (c *RuntimeDNSClient) Bind(manager *RouteRuntimeManager) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.manager = manager
	c.mu.Unlock()
}

func (*RuntimeDNSClient) Type() interface{} {
	return featuredns.ClientType()
}

func (*RuntimeDNSClient) Start() error {
	return nil
}

func (c *RuntimeDNSClient) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *RuntimeDNSClient) LookupIP(domain string, option featuredns.IPOption) ([]xnet.IP, uint32, error) {
	if c == nil {
		return nil, 0, errors.New("runtime DNS client is nil")
	}
	c.mu.RLock()
	manager := c.manager
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return nil, 0, errors.New("runtime DNS client is closed")
	}
	if manager == nil {
		return nil, 0, errors.New("runtime DNS client is not bound")
	}
	lease, err := manager.Acquire(context.Background())
	if err != nil {
		return nil, 0, err
	}
	defer lease.Release()
	backend := lease.DNS()
	if backend == nil {
		return nil, 0, errors.New("runtime DNS backend is unavailable")
	}
	return backend.LookupIP(domain, option)
}
