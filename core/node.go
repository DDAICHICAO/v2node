package core

import (
	"errors"
	"fmt"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/features/inbound"
)

func (v *V2Core) AddNode(tag string, info *panel.NodeInfo) error {
	if isSntpEclipseNode(info) {
		if v.eclipse == nil {
			v.eclipse = make(map[string]*SntpEclipseServer)
		}
		server, err := newSntpEclipseServer(tag, info, v.Server, v.dispatcher)
		if err != nil {
			return err
		}
		if err := server.Start(); err != nil {
			return err
		}
		v.eclipse[tag] = server
		return nil
	}
	if isMieruNode(info) {
		if v.mieru == nil {
			v.mieru = make(map[string]*MieruServer)
		}
		server, err := newMieruServer(tag, info, v.Server, v.dispatcher)
		if err != nil {
			return err
		}
		v.mieru[tag] = server
		return nil
	}
	inBoundConfig, err := buildInbound(info, tag)
	if err != nil {
		return fmt.Errorf("build inbound error: %s", err)
	}
	err = v.addInbound(inBoundConfig)
	if err != nil {
		return fmt.Errorf("add inbound error: %s", err)
	}
	return nil
}

func (v *V2Core) DelNode(tag string) error {
	if server, ok := v.eclipse[tag]; ok {
		delete(v.eclipse, tag)
		return server.Close()
	}
	if server, ok := v.mieru[tag]; ok {
		delete(v.mieru, tag)
		return server.Close()
	}
	err := v.removeInbound(tag)
	if err != nil {
		return fmt.Errorf("remove in error: %s", err)
	}
	return nil
}

func (v *V2Core) ReplaceNode(tag string, info *panel.NodeInfo, users []panel.UserInfo) error {
	if isSntpEclipseNode(info) || isMieruNode(info) {
		return errors.New("managed TLS runtime replacement is unsupported for this node type")
	}
	rollbackConfig, err := v.currentInboundSnapshot(tag)
	if err != nil {
		return fmt.Errorf("snapshot current inbound: %w", err)
	}
	candidate, err := v.prepareNodeInbound(tag, info, users)
	if err != nil {
		return fmt.Errorf("prepare candidate inbound: %w", err)
	}
	rollback, err := v.prepareInboundFromConfig(
		rollbackConfig,
		&AddUsersParams{Tag: tag, Users: users, NodeInfo: info},
	)
	if err != nil {
		_ = candidate.Close()
		return fmt.Errorf("prepare rollback inbound: %w", err)
	}
	return swapPreparedInbound(tag, candidate, rollback, inboundSwapHooks{
		remove: v.removeInbound,
		add:    v.addInboundHandler,
		close: func(handler inbound.Handler) error {
			return handler.Close()
		},
	})
}
