package core

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/xtls/xray-core/common"
	xraycore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/outbound"
	"google.golang.org/protobuf/proto"
)

type outboundFactory func(*xraycore.OutboundHandlerConfig) (outbound.Handler, error)

type routeOutboundPool struct {
	mu      sync.Mutex
	manager outbound.Manager
	factory outboundFactory
	entries map[string]*routeOutboundEntry
}

type routeOutboundEntry struct {
	config  *xraycore.OutboundHandlerConfig
	handler outbound.Handler
	refs    int64
}

func newRouteOutboundPool(manager outbound.Manager, factory outboundFactory) *routeOutboundPool {
	return &routeOutboundPool{
		manager: manager,
		factory: factory,
		entries: make(map[string]*routeOutboundEntry),
	}
}

func (p *routeOutboundPool) Acquire(ctx context.Context, configs []*xraycore.OutboundHandlerConfig) (map[string]outbound.Handler, func() error, error) {
	if p == nil || p.manager == nil || p.factory == nil {
		return nil, nil, errors.New("route outbound pool is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	handlers := make(map[string]outbound.Handler, len(configs))
	acquired := make([]string, 0, len(configs))
	seen := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		if config == nil || config.Tag == "" {
			_ = p.releaseLocked(context.Background(), acquired)
			return nil, nil, errors.New("route outbound config or tag is empty")
		}
		if _, duplicate := seen[config.Tag]; duplicate {
			_ = p.releaseLocked(context.Background(), acquired)
			return nil, nil, fmt.Errorf("duplicate route outbound tag %q", config.Tag)
		}
		seen[config.Tag] = struct{}{}
		if entry := p.entries[config.Tag]; entry != nil {
			if !proto.Equal(entry.config, config) {
				_ = p.releaseLocked(context.Background(), acquired)
				return nil, nil, fmt.Errorf("route outbound tag %q has conflicting config", config.Tag)
			}
			entry.refs++
			acquired = append(acquired, config.Tag)
			handlers[config.Tag] = entry.handler
			continue
		}

		handler, err := p.factory(config)
		if err != nil {
			_ = p.releaseLocked(context.Background(), acquired)
			return nil, nil, fmt.Errorf("create route outbound %q: %w", config.Tag, err)
		}
		if handler == nil || handler.Tag() != config.Tag {
			if handler != nil {
				_ = common.Close(handler)
			}
			_ = p.releaseLocked(context.Background(), acquired)
			return nil, nil, fmt.Errorf("route outbound %q returned an invalid handler", config.Tag)
		}
		if err := p.manager.AddHandler(ctx, handler); err != nil {
			_ = p.manager.RemoveHandler(context.Background(), config.Tag)
			_ = common.Close(handler)
			_ = p.releaseLocked(context.Background(), acquired)
			return nil, nil, fmt.Errorf("register route outbound %q: %w", config.Tag, err)
		}
		entry := &routeOutboundEntry{
			config:  proto.Clone(config).(*xraycore.OutboundHandlerConfig),
			handler: handler,
			refs:    1,
		}
		p.entries[config.Tag] = entry
		acquired = append(acquired, config.Tag)
		handlers[config.Tag] = handler
	}

	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			p.mu.Lock()
			releaseErr = p.releaseLocked(context.Background(), acquired)
			p.mu.Unlock()
		})
		return releaseErr
	}
	return handlers, release, nil
}

func (p *routeOutboundPool) Adopt(configs []*xraycore.OutboundHandlerConfig, handlers map[string]outbound.Handler) (func() error, error) {
	if p == nil || p.manager == nil {
		return nil, errors.New("route outbound pool is not initialized")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) != 0 {
		return nil, errors.New("route outbound pool already contains handlers")
	}
	for _, config := range configs {
		if config == nil || config.Tag == "" || handlers[config.Tag] == nil {
			return nil, errors.New("initial route outbound handler is missing")
		}
	}
	tags := make([]string, 0, len(configs))
	for _, config := range configs {
		if p.entries[config.Tag] != nil {
			for _, tag := range tags {
				delete(p.entries, tag)
			}
			return nil, fmt.Errorf("duplicate initial route outbound tag %q", config.Tag)
		}
		p.entries[config.Tag] = &routeOutboundEntry{
			config:  proto.Clone(config).(*xraycore.OutboundHandlerConfig),
			handler: handlers[config.Tag],
			refs:    1,
		}
		tags = append(tags, config.Tag)
	}
	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			p.mu.Lock()
			releaseErr = p.releaseLocked(context.Background(), tags)
			p.mu.Unlock()
		})
		return releaseErr
	}
	return release, nil
}

func (p *routeOutboundPool) releaseLocked(ctx context.Context, tags []string) error {
	var errs []error
	for index := len(tags) - 1; index >= 0; index-- {
		tag := tags[index]
		entry := p.entries[tag]
		if entry == nil || entry.refs <= 0 {
			continue
		}
		entry.refs--
		if entry.refs != 0 {
			continue
		}
		delete(p.entries, tag)
		if err := p.manager.RemoveHandler(ctx, tag); err != nil {
			errs = append(errs, fmt.Errorf("remove route outbound %q: %w", tag, err))
		}
		if err := common.Close(entry.handler); err != nil {
			errs = append(errs, fmt.Errorf("close route outbound %q: %w", tag, err))
		}
	}
	return errors.Join(errs...)
}

func (p *routeOutboundPool) Size() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}
