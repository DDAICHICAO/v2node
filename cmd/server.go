package cmd

import (
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
	//get node info
	nodes, err := node.New(c.NodeConfigs, c.StatePath)
	if err != nil {
		log.WithField("err", err).Error("Get node info failed")
		return
	}
	log.Info("Got nodes info from server")
	//core
	var reloadCh = make(chan struct{}, 1)
	v2core := core.New(c)
	v2core.ReloadCh = reloadCh
	err = v2core.Start(nodes.NodeInfos)
	if err != nil {
		log.WithField("err", err).Error("Start core failed")
		return
	}
	defer func() { _ = nodes.Close(); _ = v2core.Close() }()
	//node
	err = nodes.Start(c.NodeConfigs, v2core)
	if err != nil {
		log.WithField("err", err).Error("Run nodes failed")
		return
	}
	log.Info("Nodes started")
	if watch {
		// On file change, just signal reload; do not run reload concurrently here
		err = c.Watch(config, func() {
			select {
			case reloadCh <- struct{}{}:
			default: // drop if a reload is already queued
			}
		})
		if err != nil {
			log.WithField("err", err).Error("start watch failed")
			return
		}
	}
	// clear memory
	runtime.GC()

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-osSignals:
			log.Info("收到退出信号，正在关闭程序...")
			os.Exit(0)
		case <-reloadCh:
			log.Info("收到重启信号，正在重新加载配置...")
			if err := reload(config, &nodes, &v2core); err != nil {
				log.WithField("err", err).Error("配置切换失败")
				var fatal *reloadFatalError
				if errors.As(err, &fatal) {
					return
				}
				continue
			}
			log.Info("重启成功")
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
