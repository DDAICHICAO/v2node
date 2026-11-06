package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/features/inbound"
)

// AddNodeWithUsers loads credentials before publishing the listener. Otherwise
// incoming SS authentication scans can starve startup's per-user writes.
func (v *V2Core) AddNodeWithUsers(tag string, info *panel.NodeInfo, users []panel.UserInfo) (int, error) {
	v.nodeAccess.Lock()
	defer v.nodeAccess.Unlock()
	if info != nil && info.Type == "socks" {
		return v.addSocksNode(tag, info, users)
	}
	if isSntpEclipseNode(info) {
		if v.eclipse == nil {
			v.eclipse = make(map[string]*SntpEclipseServer)
		}
		if v.eclipse[tag] != nil {
			return 0, errors.New("inbound tag is already registered")
		}
		server, err := newSntpEclipseServer(tag, info, v.Server, v.dispatcher)
		if err != nil {
			return 0, err
		}
		server.AddUsers(users)
		if err := server.Start(); err != nil {
			_ = server.Close()
			return 0, err
		}
		v.eclipse[tag] = server
		v.commitUserUIDs(tag, users)
		v.activateUserLinks(userLinkKeys(tag, users))
		return len(users), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	existing, _ := v.ihm.GetHandler(ctx, tag)
	cancel()
	if existing != nil {
		return 0, errors.New("inbound tag is already registered")
	}
	handler, err := v.prepareNodeInbound(tag, info, users)
	if err != nil {
		return 0, fmt.Errorf("prepare inbound users: %w", err)
	}
	if err := v.addInboundHandler(handler); err != nil {
		// AddHandler may register the handler before its listener bind fails.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		installed, lookupErr := v.ihm.GetHandler(ctx, tag)
		cancel()
		if lookupErr == nil && installed == handler {
			_ = v.removeInbound(tag)
		}
		_ = handler.Close()
		v.deactivateUsers(users, tag)
		return 0, fmt.Errorf("activate prepared inbound: %w", err)
	}
	return len(users), nil
}

func (v *V2Core) AddNode(tag string, info *panel.NodeInfo) error {
	v.nodeAccess.Lock()
	defer v.nodeAccess.Unlock()
	if info != nil && info.Type == "socks" {
		_, err := v.addSocksNode(tag, info, nil)
		return err
	}
	if isSntpEclipseNode(info) {
		if v.eclipse == nil {
			v.eclipse = make(map[string]*SntpEclipseServer)
		}
		server, err := newSntpEclipseServer(tag, info, v.Server, v.dispatcher)
		if err != nil {
			return err
		}
		if err := server.Start(); err != nil {
			_ = server.Close()
			return err
		}
		v.eclipse[tag] = server
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
	v.nodeAccess.Lock()
	defer v.nodeAccess.Unlock()
	if server, ok := v.socks[tag]; ok {
		delete(v.socks, tag)
		return server.Close()
	}
	if server, ok := v.eclipse[tag]; ok {
		delete(v.eclipse, tag)
		return server.Close()
	}
	err := v.removeInbound(tag)
	if err != nil {
		return fmt.Errorf("remove in error: %s", err)
	}
	return nil
}

func (v *V2Core) ReplaceNode(tag string, info *panel.NodeInfo, users []panel.UserInfo) error {
	if info != nil && info.Type == "socks" {
		return errors.New("SOCKS5 listener changes require node reconciliation")
	}
	if isSntpEclipseNode(info) {
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
