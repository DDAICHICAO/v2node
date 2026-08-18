package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	xraycore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
)

type inboundSwapHooks struct {
	remove func(string) error
	add    func(inbound.Handler) error
	close  func(inbound.Handler) error
}

func swapPreparedInbound(
	tag string,
	candidate inbound.Handler,
	rollback inbound.Handler,
	hooks inboundSwapHooks,
) error {
	if err := hooks.remove(tag); err != nil {
		_ = hooks.close(candidate)
		_ = hooks.close(rollback)
		return fmt.Errorf("remove current inbound: %w", err)
	}
	if err := hooks.add(candidate); err == nil {
		_ = hooks.close(rollback)
		return nil
	} else {
		cleanupErr := hooks.remove(tag)
		if cleanupErr != nil {
			_ = hooks.close(candidate)
		}
		if rollbackErr := hooks.add(rollback); rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("activate candidate inbound: %w", err),
				cleanupErr,
				fmt.Errorf("restore previous inbound: %w", rollbackErr),
			)
		}
		return errors.Join(
			fmt.Errorf("activate candidate inbound: %w", err),
			cleanupErr,
		)
	}
}

func (v *V2Core) prepareNodeInbound(
	tag string,
	info *panel.NodeInfo,
	users []panel.UserInfo,
) (inbound.Handler, error) {
	config, err := buildInbound(info, tag)
	if err != nil {
		return nil, fmt.Errorf("build inbound: %w", err)
	}
	return v.prepareInboundFromConfig(
		config,
		&AddUsersParams{Tag: tag, Users: users, NodeInfo: info},
	)
}

func (v *V2Core) prepareInboundFromConfig(
	config *xraycore.InboundHandlerConfig,
	params *AddUsersParams,
) (inbound.Handler, error) {
	handler, err := v.createInboundHandler(config)
	if err != nil {
		return nil, err
	}
	manager, err := inboundUserManager(handler, params.Tag)
	if err != nil {
		_ = handler.Close()
		return nil, err
	}
	users, err := buildCoreUsers(params)
	if err != nil {
		_ = handler.Close()
		return nil, err
	}
	if _, err := v.addManagedUsers(manager, params.Tag, params.Users, users); err != nil {
		_ = handler.Close()
		return nil, err
	}
	return handler, nil
}

func (v *V2Core) currentInboundSnapshot(tag string) (*xraycore.InboundHandlerConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handler, err := v.ihm.GetHandler(ctx, tag)
	if err != nil {
		return nil, err
	}
	return &xraycore.InboundHandlerConfig{
		Tag:              tag,
		ReceiverSettings: handler.ReceiverSettings(),
		ProxySettings:    handler.ProxySettings(),
	}, nil
}
