package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

type Node struct {
	deletedConfigs    []conf.NodeConfig
	controllers       []*Controller
	NodeInfos         []*panel.NodeInfo
	routeStateStore   *routeRuntimeStateStore
	routeRuntimeState *routeRuntimeState
	routeCoordinator  *routeUpdateCoordinator
}

func New(nodes []conf.NodeConfig, statePath string) (*Node, error) {
	routeStore := newRouteRuntimeStateStore(statePath)
	store := newOfflineStateStore(statePath)
	var runtimeState *routeRuntimeState
	var runtimeEntries map[string]routeRuntimeEntry
	if loaded, err := routeStore.Load(); err == nil {
		// Check tombstones only for missing generation entries, avoiding another
		// serial read of every node's potentially large user snapshot at startup.
		present := make(map[string]bool, len(loaded.Entries))
		for _, entry := range loaded.Entries {
			present[routeRuntimeIdentity(entry.APIHost, entry.NodeID)] = true
		}
		snapshotConfigs := make([]conf.NodeConfig, 0, len(nodes))
		for _, cfg := range nodes {
			if !present[routeRuntimeIdentity(cfg.APIHost, cfg.NodeID)] {
				if cached, err := store.Load(cfg); err == nil && cached.Deleted {
					continue
				}
			}
			snapshotConfigs = append(snapshotConfigs, cfg)
		}
		entries, identityErr := matchRouteRuntimeEntries(loaded, snapshotConfigs)
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
	started := time.Now()
	errorsByNode := make([]error, len(nodes))
	slots := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, node := range nodes {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			controller, err := prepareController(node, store, runtimeEntries)
			errorsByNode[i] = err
			if err == nil {
				n.controllers[i] = controller
				n.NodeInfos[i] = controller.bootstrap.NodeInfo
			}
		})
	}
	wg.Wait()
	controllers := n.controllers[:0]
	infos := n.NodeInfos[:0]
	for i, err := range errorsByNode {
		if errors.Is(err, panel.ErrNodeNotFound) {
			n.deletedConfigs = append(n.deletedConfigs, nodes[i])
			log.WithField("node_id", nodes[i].NodeID).Warn("Skipping deleted panel node")
			continue
		}
		if err != nil {
			return nil, err
		}
		controllers = append(controllers, n.controllers[i])
		infos = append(infos, n.NodeInfos[i])
	}
	n.controllers, n.NodeInfos = controllers, infos
	logBootstrapDuration(0, "all_nodes", started)
	return n, nil
}

func prepareController(node conf.NodeConfig, store *offlineStateStore, runtimeEntries map[string]routeRuntimeEntry) (*Controller, error) {
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
		if errors.Is(err, panel.ErrNodeNotFound) {
			if saveErr := store.SaveDeleted(node); saveErr != nil {
				return nil, fmt.Errorf("persist node %d deletion: %w", node.NodeID, saveErr)
			}
		}
		return nil, fmt.Errorf("load node [%s-%d] bootstrap state: %w", node.APIHost, node.NodeID, err)
	}
	controller := NewController(p, &node, store, bootstrap, startedOffline)
	if p.NodeInfoVersion() == "" {
		if entry, ok := runtimeEntries[routeRuntimeIdentity(node.APIHost, node.NodeID)]; ok {
			controller.activeNodeInfoVersion = entry.ConfigVersion
		}
	}
	return controller, nil
}

func (n *Node) Start(nodes []conf.NodeConfig, v2core *core.V2Core) error {
	for _, controller := range n.controllers {
		controller.routeCoordinator = nil
	}
	var coordinator *routeUpdateCoordinator
	if routeHotReloadEnabled(v2core) {
		targets := make([]routeUpdateTarget, len(n.controllers))
		for index, controller := range n.controllers {
			targets[index] = controller
		}
		coordinator = newRouteUpdateCoordinator(
			targets,
			v2core,
			n.routeStateStore,
			routeUpdateCoordinatorOptions{},
		)
		for _, controller := range n.controllers {
			controller.routeCoordinator = coordinator
		}
		coordinator.Start()
	}
	n.routeCoordinator = coordinator
	for _, controller := range n.controllers {
		err := controller.Start(v2core)
		if err != nil {
			if coordinator != nil {
				coordinator.Close()
			}
			return fmt.Errorf("start node controller [%s-%d] error: %s",
				controller.conf.APIHost,
				controller.conf.NodeID,
				err)
		}
	}
	return nil
}

func routeHotReloadEnabled(v2core *core.V2Core) bool {
	return v2core != nil && v2core.Config != nil && v2core.Config.EnableRouteHotReload
}

func (n *Node) Close() error {
	var err error
	if n.routeCoordinator != nil {
		n.routeCoordinator.Close()
		n.routeCoordinator = nil
	}
	for _, c := range n.controllers {
		if err = c.Close(); err != nil {
			log.Errorf("close controller failed: %v", err)
			return err
		}
	}
	return nil
}
