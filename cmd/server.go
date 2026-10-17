package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
	"github.com/wyx2685/v2node/node"
)

var (
	config string
	watch  bool
)

var serverCommand = cobra.Command{
	Use:   "server",
	Short: "Run v2node server",
	Run:   serverHandle,
	Args:  cobra.NoArgs,
}

func init() {
	serverCommand.PersistentFlags().
		StringVarP(&config, "config", "c",
			"/etc/v2node/config.json", "config file path")
	serverCommand.PersistentFlags().
		BoolVarP(&watch, "watch", "w",
			true, "watch file path change")
	command.AddCommand(&serverCommand)
}

func serverHandle(_ *cobra.Command, _ []string) {
	showVersion()
	c := conf.New()
	err := c.LoadFromPath(config)
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: true,
		DisableQuote:     true,
		PadLevelText:     false,
	})
	if err != nil {
		log.WithField("err", err).Error("Load config file failed")
		return
	}
	applyLogConfig(c.LogConfig)
	// Enable pprof if configured
	if c.PprofPort != 0 {
		go func() {
			log.Infof("Starting pprof server on :%d", c.PprofPort)
			if err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", c.PprofPort), nil); err != nil {
				log.WithField("err", err).Error("pprof server failed")
			}
		}()
	}
	//init limiter
	limiter.Init()
	// Management is independent of every business controller and starts before bootstrap.
	ctx, cancelControl := context.WithCancel(context.Background())
	defer cancelControl()
	manager, err := node.NewCleanupManager(config)
	if err != nil {
		log.Error("Initialize node management failed")
		return
	}
	go manager.Run(ctx)
	reloadCh := make(chan struct{}, 1)
	fileChangeCh := make(chan struct{}, 1)
	var nodes *node.Node
	var v2core *core.V2Core
	lastHandledDigest := ""
	startRuntime := func() error {
		preparedDigest := manager.ConfigDigest()
		candidateConf := conf.New()
		if err := candidateConf.LoadFromPath(config); err != nil {
			return err
		}
		candidate, err := node.New(candidateConf.NodeConfigs, candidateConf.StatePath)
		if err != nil {
			return err
		}
		server := core.New(candidateConf)
		server.ReloadCh = reloadCh
		if err = server.Start(candidate.NodeInfos); err == nil {
			err = candidate.Start(candidateConf.NodeConfigs, server)
		}
		if err != nil {
			cleanupErr := errors.Join(candidate.Close(), server.Close())
			if cleanupErr != nil {
				return &reloadFatalError{errors.Join(err, cleanupErr)}
			}
			return err
		}
		nodes, v2core = candidate, server
		applyLogConfig(candidateConf.LogConfig)
		lastHandledDigest = preparedDigest
		return nil
	}
	if err := startRuntime(); err != nil {
		var fatal *reloadFatalError
		if errors.As(err, &fatal) {
			log.WithError(err).Error("启动失败且运行态无法清理，退出进程")
			return
		}
		log.WithField("err", err).Error("Business nodes failed to start; management remains active and startup will retry")
	}
	defer func() {
		if nodes != nil {
			_ = nodes.Close()
		}
		if v2core != nil {
			_ = v2core.Close()
		}
	}()
	if watch {
		if err := c.Watch(config, func() {
			select {
			case fileChangeCh <- struct{}{}:
			default:
			}
		}); err != nil {
			log.WithField("err", err).Error("start watch failed")
		}
	}
	runtime.GC()
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(osSignals)
	retry := time.NewTicker(30 * time.Second)
	defer retry.Stop()
	for {
		select {
		case <-osSignals:
			log.Info("收到退出信号，正在关闭程序...")
			return
		case req := <-manager.Requests:
			manager.Execute(ctx, req, nodes, v2core)
			if req.AppliedDigest != "" {
				lastHandledDigest = req.AppliedDigest
			}
		case <-retry.C:
			if nodes == nil {
				if err := startRuntime(); err != nil {
					var fatal *reloadFatalError
					if errors.As(err, &fatal) {
						log.WithError(err).Error("启动失败且运行态无法清理，退出进程")
						return
					}
					log.WithField("err", err).Warn("业务节点启动待重试，管理通道继续运行")
				}
			}
		case <-fileChangeCh:
			if digest := manager.ConfigDigest(); digest != "" && digest == lastHandledDigest {
				continue
			}
			select {
			case reloadCh <- struct{}{}:
			default:
			}
		case <-reloadCh:
			if nodes == nil {
				if err := startRuntime(); err != nil {
					var fatal *reloadFatalError
					if errors.As(err, &fatal) {
						log.WithError(err).Error("启动失败且运行态无法清理，退出进程")
						return
					}
					log.WithField("err", err).Warn("业务节点启动待重试")
				}
				continue
			}
			digest := manager.ConfigDigest()
			if err := reload(config, &nodes, &v2core); err != nil {
				log.WithField("err", err).Error("配置切换失败")
				var fatal *reloadFatalError
				if errors.As(err, &fatal) {
					return
				}
				continue
			}
			lastHandledDigest = digest
			log.Info("配置更新成功")
		}
	}
}

type reloadFatalError struct{ err error }

func (e *reloadFatalError) Error() string { return e.err.Error() }
func (e *reloadFatalError) Unwrap() error { return e.err }

func reload(config string, nodes **node.Node, v2core **core.V2Core) error {
	started := time.Now()
	newConf := conf.New()
	if err := newConf.LoadFromPath(config); err != nil {
		return err
	}
	if _, err := newConf.AccessAuditConfig.RuntimeConfig(); err != nil {
		return err
	}
	newNodes, err := node.New(newConf.NodeConfigs, newConf.StatePath)
	if err != nil {
		return err
	}
	if err := newNodes.Validate(*v2core); err != nil {
		return err
	}
	if err := (*nodes).RefreshPreparedUsers(newNodes); err != nil {
		return err
	}
	log.WithField("elapsed_ms", time.Since(started).Milliseconds()).Info("SNTP reload candidate prepared while active nodes kept serving")
	oldConfValue := *(*v2core).Config
	oldConfValue.NodeConfigs = (*nodes).Configs()
	oldConf := &oldConfValue
	oldGlobal, newGlobal := *oldConf, *newConf
	oldGlobal.NodeConfigs, newGlobal.NodeConfigs = nil, nil
	if reflect.DeepEqual(oldGlobal, newGlobal) {
		applied, err := (*nodes).Reconcile(newNodes, *v2core)
		if applied {
			var rollbackErr *node.RollbackError
			if errors.As(err, &rollbackErr) {
				return &reloadFatalError{err}
			}
			if err == nil {
				applyLogConfig(newConf.LogConfig)
			}
			return err
		}
	}
	oldReloadCh := (*v2core).ReloadCh
	oldNodes := *nodes
	switchStarted := time.Now()
	if err := oldNodes.Close(); err != nil {
		return &reloadFatalError{err}
	}
	oldNodes.InheritStoppedUsers(newNodes)
	rollbackNodes, err := oldNodes.RestartCopy()
	if err != nil {
		return &reloadFatalError{err}
	}
	if err := (*v2core).Close(); err != nil {
		return &reloadFatalError{err}
	}
	newCore := core.New(newConf)
	newCore.ReloadCh = oldReloadCh
	activationErr := newCore.Start(newNodes.NodeInfos)
	if activationErr == nil {
		activationErr = newNodes.Start(newConf.NodeConfigs, newCore)
	}
	if activationErr != nil {
		_ = newNodes.Close()
		_ = newCore.Close()
		rollbackCore := core.New(oldConf)
		rollbackCore.ReloadCh = oldReloadCh
		restoreErr := rollbackCore.Start(rollbackNodes.NodeInfos)
		if restoreErr == nil {
			restoreErr = rollbackNodes.Start(oldConf.NodeConfigs, rollbackCore)
		}
		*nodes, *v2core = rollbackNodes, rollbackCore
		if restoreErr != nil {
			return &reloadFatalError{errors.Join(activationErr, fmt.Errorf("restore previous core: %w", restoreErr))}
		}
		return fmt.Errorf("candidate activation failed; previous runtime restored: %w", activationErr)
	}
	*nodes, *v2core = newNodes, newCore
	applyLogConfig(newConf.LogConfig)
	log.WithField("elapsed_ms", time.Since(switchStarted).Milliseconds()).Info("SNTP shared core configuration switched")
	runtime.GC()
	return nil
}

func applyLogConfig(logConfig conf.LogConfig) {
	switch logConfig.Level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.SetLevel(log.WarnLevel)
	}

	nextOutput := log.StandardLogger().Out
	if logConfig.Output == "" {
		nextOutput = os.Stdout
	} else {
		f, err := os.OpenFile(logConfig.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.WithField("err", err).Error("Open log file failed, using stdout instead")
			nextOutput = os.Stdout
		} else {
			nextOutput = f
		}
	}

	if oldWriter, ok := log.StandardLogger().Out.(*os.File); ok && oldWriter != os.Stdout && oldWriter != os.Stderr && oldWriter != nextOutput {
		_ = oldWriter.Close()
	}
	log.SetOutput(nextOutput)
}
