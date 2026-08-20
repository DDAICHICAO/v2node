package node

import (
	"context"
	"errors"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

type Node struct {
	controllers       []*Controller
	NodeInfos         []*panel.NodeInfo
	routeStateStore   *routeRuntimeStateStore
	routeRuntimeState *routeRuntimeState
}

func New(nodes []conf.NodeConfig, statePath string) (*Node, error) {
	routeStore := newRouteRuntimeStateStore(statePath)
	var runtimeState *routeRuntimeState
	var runtimeEntries map[string]routeRuntimeEntry
	if loaded, err := routeStore.Load(); err == nil {
		entries, identityErr := matchRouteRuntimeEntries(loaded, nodes)
		if identityErr != nil {
			log.WithError(identityErr).Warning("Whole-machine route runtime snapshot does not match configured nodes")
		} else {
			runtimeState = loaded
			runtimeEntries = entries
		}
	} else if errors.Is(err, os.ErrNotExist) {
		log.Debug("Whole-machine route runtime snapshot does not exist")
	} else {
		log.WithError(err).Warning("Whole-machine route runtime snapshot is invalid")
	}
	n := &Node{
		controllers:       make([]*Controller, len(nodes)),
		NodeInfos:         make([]*panel.NodeInfo, len(nodes)),
		routeStateStore:   routeStore,
		routeRuntimeState: runtimeState,
	}
	store := newOfflineStateStore(statePath)
	for i, node := range nodes {
		p, err := panel.New(&node)
		if err != nil {
			return nil, err
		}
		cached, err := store.Load(node)
		if err != nil {
			cached = nil
			if errors.Is(err, os.ErrNotExist) {
				log.WithFields(log.Fields{
					"api_host": normalizeAPIHost(node.APIHost),
					"node_id":  node.NodeID,
				}).Debug("Offline snapshot does not exist")
			} else {
				log.WithFields(log.Fields{
					"api_host": normalizeAPIHost(node.APIHost),
					"node_id":  node.NodeID,
					"err":      err,
				}).Warning("Offline snapshot is invalid; trying panel state")
			}
		}
		if cached != nil {
			if entry, ok := runtimeEntries[routeRuntimeIdentity(node.APIHost, node.NodeID)]; ok {
				if !overlayRouteRuntimeState(cached, entry) {
					log.WithFields(log.Fields{
						"api_host": normalizeAPIHost(node.APIHost),
						"node_id":  node.NodeID,
					}).Warning("Whole-machine route runtime snapshot entry could not overlay offline state")
				}
			}
		}
		bootstrap, startedOffline, err := loadBootstrapState(context.Background(), p, node, cached)
		if err != nil {
			return nil, fmt.Errorf("load node [%s-%d] bootstrap state: %w", node.APIHost, node.NodeID, err)
		}
		n.controllers[i] = NewController(p, &node, store, bootstrap, startedOffline)
		n.NodeInfos[i] = bootstrap.NodeInfo
	}
	return n, nil
}

func (n *Node) Start(nodes []conf.NodeConfig, core *core.V2Core) error {
	for i, node := range nodes {
		err := n.controllers[i].Start(core)
		if err != nil {
			return fmt.Errorf("start node controller [%s-%d] error: %s",
				node.APIHost,
				node.NodeID,
				err)
		}
	}
	return nil
}

func (n *Node) Close() error {
	var err error
	for _, c := range n.controllers {
		if err = c.Close(); err != nil {
			log.Errorf("close controller failed: %v", err)
			return err
		}
	}
	n.controllers = nil
	return nil
}
