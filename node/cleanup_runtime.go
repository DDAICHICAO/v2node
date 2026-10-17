package node

import (
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

// Called only by the server's owner loop. Never prepares/restarts unrelated controllers.
func (n *Node) removeOrphanRuntime(scope controlScope, nodeID int, server *core.V2Core) error {
	if n.routeCoordinator != nil {
		n.routeCoordinator.Close()
		n.routeCoordinator = nil
	}
	defer n.resetCoordinator(server)
	kept := make([]*Controller, 0, len(n.controllers))
	for _, c := range n.controllers {
		if normalizeAPIHost(c.conf.APIHost) != scope.Host || c.conf.Key != scope.Key || c.conf.NodeID != nodeID {
			kept = append(kept, c)
			continue
		}
		c.configFetchMu.Lock()
		c.markDeleted()
		c.configFetchMu.Unlock()
		if err := c.RetireDeletedNode(); err != nil {
			return err
		}
	}
	n.controllers = kept
	n.refreshNodeInfos()
	// Route compilation also supports an empty generation. Retiring the listener alone
	// is insufficient when its routing rules are shared with surviving listeners.
	if _, err := server.ApplyRouteRuntime(n.NodeInfos); err != nil {
		return err
	}
	filter := func(configs []conf.NodeConfig) []conf.NodeConfig {
		out := make([]conf.NodeConfig, 0, len(configs))
		for _, cfg := range configs {
			if normalizeAPIHost(cfg.APIHost) != scope.Host || cfg.Key != scope.Key || cfg.NodeID != nodeID {
				out = append(out, cfg)
			}
		}
		return out
	}
	n.deletedConfigs = filter(n.deletedConfigs)
	server.Config.NodeConfigs = filter(server.Config.NodeConfigs)
	return nil
}
