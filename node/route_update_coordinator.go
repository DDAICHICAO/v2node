package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

const (
	defaultRouteStabilizationWindow = 500 * time.Millisecond
	defaultRouteSweepInterval       = 250 * time.Millisecond
	defaultRouteInitialBackoff      = time.Second
	defaultRouteMaxBackoff          = 30 * time.Second
	defaultRouteMaxConcurrency      = 4
)

var (
	errRouteCoordinatorInterrupted = errors.New("route update coordination interrupted")
	errRouteVectorChanged          = errors.New("route configuration vector changed during stabilization")
)

type routeObservedState struct {
	NodeID   int
	APIHost  string
	Version  string
	Active   *panel.NodeInfo
	Observed *panel.NodeInfo
}

type routeUpdateTarget interface {
	RefreshObserved(context.Context) (routeObservedState, error)
	ObservedState() routeObservedState
	CommitRouteNodeInfo(*panel.NodeInfo, string) error
}

type routeRuntimeApplier interface {
	ApplyRouteRuntime([]*panel.NodeInfo) (uint64, error)
}

type routeRuntimeStateWriter interface {
	Save(*routeRuntimeState) error
}

type routeUpdateCoordinatorOptions struct {
	StabilizationWindow time.Duration
	SweepInterval       time.Duration
	InitialBackoff      time.Duration
	MaxBackoff          time.Duration
	MaxConcurrency      int
	Jitter              func(time.Duration) time.Duration
}

type routePublishedBatch struct {
	generation    uint64
	epoch         uint64
	vectorHash    string
	states        []routeObservedState
	startedAt     time.Time
	snapshotSaved bool
}

type routeUpdateCoordinatorStats struct {
	Triggered uint64
	Coalesced uint64
	Attempts  uint64
	Successes uint64
	Failures  uint64
	Stale     uint64
}

type routeUpdateCoordinator struct {
	targets []routeUpdateTarget
	applier routeRuntimeApplier
	writer  routeRuntimeStateWriter
	options routeUpdateCoordinatorOptions

	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	wake      chan struct{}
	epoch     atomic.Uint64
	triggered atomic.Uint64
	coalesced atomic.Uint64
	attempts  atomic.Uint64
	successes atomic.Uint64
	failures  atomic.Uint64
	stale     atomic.Uint64

	startOnce sync.Once
	closeOnce sync.Once
}

func newRouteUpdateCoordinator(
	targets []routeUpdateTarget,
	applier routeRuntimeApplier,
	writer routeRuntimeStateWriter,
	options routeUpdateCoordinatorOptions,
) *routeUpdateCoordinator {
	ctx, cancel := context.WithCancel(context.Background())
	return &routeUpdateCoordinator{
		targets: append([]routeUpdateTarget(nil), targets...),
		applier: applier,
		writer:  writer,
		options: normalizeRouteUpdateCoordinatorOptions(options),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		wake:    make(chan struct{}, 1),
	}
}

func normalizeRouteUpdateCoordinatorOptions(options routeUpdateCoordinatorOptions) routeUpdateCoordinatorOptions {
	if options.StabilizationWindow <= 0 {
		options.StabilizationWindow = defaultRouteStabilizationWindow
	}
	if options.SweepInterval <= 0 {
		options.SweepInterval = defaultRouteSweepInterval
	}
	if options.InitialBackoff <= 0 {
		options.InitialBackoff = defaultRouteInitialBackoff
	}
	if options.MaxBackoff <= 0 {
		options.MaxBackoff = defaultRouteMaxBackoff
	}
	if options.InitialBackoff > options.MaxBackoff {
		options.InitialBackoff = options.MaxBackoff
	}
	if options.MaxConcurrency <= 0 {
		options.MaxConcurrency = defaultRouteMaxConcurrency
	}
	if options.Jitter == nil {
		options.Jitter = func(delay time.Duration) time.Duration {
			if delay <= 0 {
				return 0
			}
			// Keep retries spread between 80% and 120% of the capped delay.
			factor := 0.8 + rand.Float64()*0.4
			return time.Duration(float64(delay) * factor)
		}
	}
	return options
}

func (c *routeUpdateCoordinator) Start() {
	if c == nil {
		return
	}
	c.startOnce.Do(func() {
		go c.run()
	})
}

func (c *routeUpdateCoordinator) Notify() {
	if c == nil {
		return
	}
	c.epoch.Add(1)
	c.triggered.Add(1)
	select {
	case c.wake <- struct{}{}:
	default:
		c.coalesced.Add(1)
	}
}

func (c *routeUpdateCoordinator) Stats() routeUpdateCoordinatorStats {
	if c == nil {
		return routeUpdateCoordinatorStats{}
	}
	return routeUpdateCoordinatorStats{
		Triggered: c.triggered.Load(),
		Coalesced: c.coalesced.Load(),
		Attempts:  c.attempts.Load(),
		Successes: c.successes.Load(),
		Failures:  c.failures.Load(),
		Stale:     c.stale.Load(),
	}
}

func (c *routeUpdateCoordinator) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.cancel()
		// Ensure done is eventually closed even when Close precedes Start.
		c.Start()
		<-c.done
	})
}

func (c *routeUpdateCoordinator) run() {
	defer close(c.done)
	var published *routePublishedBatch
	for {
		if published == nil {
			select {
			case <-c.ctx.Done():
				return
			case <-c.wake:
			}
			if !c.waitForQuietWindow() {
				return
			}
		}

		backoff := c.options.InitialBackoff
		for {
			var err error
			if published != nil {
				err = c.commitPublished(published)
			} else {
				published, err = c.reconcile()
				if err == nil && published == nil {
					break
				}
				if err == nil {
					err = c.commitPublished(published)
				}
			}

			if err == nil {
				handledEpoch := published.epoch
				log.WithFields(log.Fields{
					"generation":  published.generation,
					"nodes":       len(published.states),
					"vector_hash": shortRouteVectorHash(published.vectorHash),
					"duration":    time.Since(published.startedAt).Round(time.Millisecond),
				}).Info("Route runtime generation published")
				published = nil
				if c.epoch.Load() != handledEpoch {
					if !c.waitForQuietWindow() {
						return
					}
					backoff = c.options.InitialBackoff
					continue
				}
				break
			}

			if c.ctx.Err() != nil {
				return
			}
			if errors.Is(err, errRouteCoordinatorInterrupted) || errors.Is(err, errRouteVectorChanged) {
				c.stale.Add(1)
				if !c.waitForQuietWindow() {
					return
				}
				backoff = c.options.InitialBackoff
				continue
			}
			c.failures.Add(1)
			fields := log.Fields{
				"err":      err,
				"backoff":  backoff,
				"attempts": c.attempts.Load(),
			}
			if published != nil {
				fields["generation"] = published.generation
				fields["nodes"] = len(published.states)
				fields["vector_hash"] = shortRouteVectorHash(published.vectorHash)
				fields["duration"] = time.Since(published.startedAt).Round(time.Millisecond)
			}
			log.WithFields(fields).Warning("Route runtime coordination failed; keeping active generation")
			interrupted, ok := c.waitForRetry(backoff)
			if !ok {
				return
			}
			if interrupted && !c.waitForQuietWindow() {
				return
			}
			backoff = nextRouteBackoff(backoff, c.options.MaxBackoff)
		}
	}
}

func (c *routeUpdateCoordinator) reconcile() (*routePublishedBatch, error) {
	startedAt := time.Now()
	c.attempts.Add(1)
	if len(c.targets) == 0 || c.applier == nil || c.writer == nil {
		return nil, errors.New("route update coordinator dependencies are incomplete")
	}
	epoch := c.epoch.Load()
	_, firstHash, err := c.refreshVector()
	if err != nil {
		return nil, err
	}
	if !c.waitForSweepInterval() {
		return nil, errRouteCoordinatorInterrupted
	}
	second, secondHash, err := c.refreshVector()
	if err != nil {
		return nil, err
	}
	if firstHash != secondHash {
		return nil, errRouteVectorChanged
	}
	if c.epoch.Load() != epoch {
		return nil, errRouteCoordinatorInterrupted
	}
	current, currentHash, err := c.currentVector()
	if err != nil {
		return nil, err
	}
	if currentHash != secondHash {
		return nil, errRouteVectorChanged
	}
	states := current
	if len(states) != len(second) {
		return nil, errRouteVectorChanged
	}

	hasRouteChange := false
	infos := make([]*panel.NodeInfo, 0, len(states))
	clonedStates := make([]routeObservedState, 0, len(states))
	for _, state := range states {
		kind := classifyNodeInfoChange(state.Active, state.Observed)
		switch kind {
		case nodeInfoUnchanged:
		case nodeInfoRoutesOnly:
			hasRouteChange = true
		default:
			return nil, fmt.Errorf("node %d has non-route configuration change", state.NodeID)
		}
		clone, cloneErr := cloneRouteRuntimeNodeInfo(state.Observed)
		if cloneErr != nil {
			return nil, fmt.Errorf("clone node %d route configuration: %w", state.NodeID, cloneErr)
		}
		state.Observed = clone
		infos = append(infos, clone)
		clonedStates = append(clonedStates, state)
	}
	if !hasRouteChange {
		return nil, nil
	}
	if c.epoch.Load() != epoch {
		return nil, errRouteCoordinatorInterrupted
	}
	generation, err := safeApplyRouteRuntime(c.applier, infos)
	if err != nil {
		return nil, err
	}
	c.successes.Add(1)
	return &routePublishedBatch{
		generation: generation,
		epoch:      epoch,
		vectorHash: currentHash,
		states:     clonedStates,
		startedAt:  startedAt,
	}, nil
}

func (c *routeUpdateCoordinator) refreshVector() ([]routeObservedState, string, error) {
	states := make([]routeObservedState, len(c.targets))
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	semaphore := make(chan struct{}, c.options.MaxConcurrency)
	errCh := make(chan error, len(c.targets))
	var wg sync.WaitGroup
	for index, target := range c.targets {
		wg.Add(1)
		go func(index int, target routeUpdateTarget) {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
			defer func() { <-semaphore }()
			state, err := safeRefreshRouteTarget(ctx, target)
			if err != nil {
				errCh <- fmt.Errorf("refresh route target: %w", err)
				cancel()
				return
			}
			states[index] = state
		}(index, target)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return nil, "", err
		}
	}
	return normalizeRouteObservedVector(states)
}

func (c *routeUpdateCoordinator) currentVector() ([]routeObservedState, string, error) {
	states := make([]routeObservedState, len(c.targets))
	for index, target := range c.targets {
		state, err := safeObservedRouteTarget(target)
		if err != nil {
			return nil, "", err
		}
		states[index] = state
	}
	return normalizeRouteObservedVector(states)
}

func normalizeRouteObservedVector(states []routeObservedState) ([]routeObservedState, string, error) {
	states = append([]routeObservedState(nil), states...)
	sort.Slice(states, func(i, j int) bool {
		if states[i].NodeID != states[j].NodeID {
			return states[i].NodeID < states[j].NodeID
		}
		return normalizeAPIHost(states[i].APIHost) < normalizeAPIHost(states[j].APIHost)
	})
	hash := sha256.New()
	seen := make(map[string]struct{}, len(states))
	for index := range states {
		state := &states[index]
		state.APIHost = normalizeAPIHost(state.APIHost)
		state.Version = strings.TrimSpace(state.Version)
		if state.NodeID <= 0 || state.APIHost == "" || state.Version == "" || state.Active == nil || state.Observed == nil {
			return nil, "", errors.New("route target observed state is incomplete")
		}
		identity := routeRuntimeIdentity(state.APIHost, state.NodeID)
		if _, duplicate := seen[identity]; duplicate {
			return nil, "", fmt.Errorf("duplicate route target identity %s", identity)
		}
		seen[identity] = struct{}{}
		_, _ = hash.Write([]byte(strconv.Itoa(len(identity))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(identity))
		_, _ = hash.Write([]byte(strconv.Itoa(len(state.Version))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(state.Version))
	}
	return states, hex.EncodeToString(hash.Sum(nil)), nil
}

func (c *routeUpdateCoordinator) commitPublished(batch *routePublishedBatch) error {
	if batch == nil {
		return errors.New("published route batch is nil")
	}
	if !batch.snapshotSaved {
		entries := make([]routeRuntimeEntry, 0, len(batch.states))
		for _, observed := range batch.states {
			clone, err := cloneRouteRuntimeNodeInfo(observed.Observed)
			if err != nil {
				return fmt.Errorf("clone published node %d route configuration: %w", observed.NodeID, err)
			}
			entries = append(entries, routeRuntimeEntry{
				APIHost:       normalizeAPIHost(observed.APIHost),
				NodeID:        observed.NodeID,
				ConfigVersion: observed.Version,
				NodeInfo:      clone,
			})
		}
		state := &routeRuntimeState{
			Version:    routeRuntimeStateVersion,
			Generation: batch.generation,
			SavedAt:    time.Now().Unix(),
			VectorHash: batch.vectorHash,
			Entries:    entries,
		}
		if err := safeSaveRouteRuntimeState(c.writer, state); err != nil {
			return fmt.Errorf("save whole-machine route runtime state: %w", err)
		}
		batch.snapshotSaved = true
	}

	targets, err := c.targetsByIdentity()
	if err != nil {
		return err
	}
	for _, observed := range batch.states {
		identity := routeRuntimeIdentity(observed.APIHost, observed.NodeID)
		target := targets[identity]
		if target == nil {
			return fmt.Errorf("route target %s is unavailable", identity)
		}
		if err := safeCommitRouteTarget(target, observed.Observed, observed.Version); err != nil {
			return fmt.Errorf("commit node %d route configuration: %w", observed.NodeID, err)
		}
	}
	return nil
}

func (c *routeUpdateCoordinator) targetsByIdentity() (map[string]routeUpdateTarget, error) {
	targets := make(map[string]routeUpdateTarget, len(c.targets))
	for _, target := range c.targets {
		observed, err := safeObservedRouteTarget(target)
		if err != nil {
			return nil, err
		}
		if observed.NodeID <= 0 || normalizeAPIHost(observed.APIHost) == "" {
			return nil, errors.New("route target identity is incomplete")
		}
		identity := routeRuntimeIdentity(observed.APIHost, observed.NodeID)
		if targets[identity] != nil {
			return nil, fmt.Errorf("duplicate route target identity %s", identity)
		}
		targets[identity] = target
	}
	return targets, nil
}

func safeRefreshRouteTarget(ctx context.Context, target routeUpdateTarget) (state routeObservedState, err error) {
	defer func() {
		if recover() != nil {
			state = routeObservedState{}
			err = errors.New("route target refresh panicked")
		}
	}()
	return target.RefreshObserved(ctx)
}

func safeObservedRouteTarget(target routeUpdateTarget) (state routeObservedState, err error) {
	defer func() {
		if recover() != nil {
			state = routeObservedState{}
			err = errors.New("route target state read panicked")
		}
	}()
	return target.ObservedState(), nil
}

func safeCommitRouteTarget(target routeUpdateTarget, info *panel.NodeInfo, version string) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("route target commit panicked")
		}
	}()
	return target.CommitRouteNodeInfo(info, version)
}

func safeApplyRouteRuntime(applier routeRuntimeApplier, infos []*panel.NodeInfo) (generation uint64, err error) {
	defer func() {
		if recover() != nil {
			generation = 0
			err = errors.New("route runtime apply panicked")
		}
	}()
	return applier.ApplyRouteRuntime(infos)
}

func safeSaveRouteRuntimeState(writer routeRuntimeStateWriter, state *routeRuntimeState) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("route runtime state save panicked")
		}
	}()
	return writer.Save(state)
}

func (c *routeUpdateCoordinator) waitForQuietWindow() bool {
	timer := time.NewTimer(c.options.StabilizationWindow)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return false
		case <-c.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(c.options.StabilizationWindow)
		case <-timer.C:
			return true
		}
	}
}

func (c *routeUpdateCoordinator) waitForSweepInterval() bool {
	timer := time.NewTimer(c.options.SweepInterval)
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return false
	case <-c.wake:
		return false
	case <-timer.C:
		return true
	}
}

func (c *routeUpdateCoordinator) waitForRetry(delay time.Duration) (interrupted bool, ok bool) {
	delay = c.options.Jitter(delay)
	if delay < 0 {
		delay = 0
	}
	if delay > c.options.MaxBackoff {
		delay = c.options.MaxBackoff
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return false, false
	case <-c.wake:
		return true, true
	case <-timer.C:
		return false, true
	}
}

func nextRouteBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum/2 {
		return maximum
	}
	return current * 2
}

func shortRouteVectorHash(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}
