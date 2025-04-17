package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"meoe.io/cgroup-burst/internal/appconfig"
	"meoe.io/cgroup-burst/internal/k8swatcher"

	slogctx "github.com/veqryn/slog-context"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	appConfig := appconfig.ParseConfig()

	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	logger = logger.With("host", appConfig.Hostname)
	ctx = slogctx.NewCtx(ctx, logger)

	logger.Info("using config", "config", appConfig)

	// set log level after the 'using config' log entry, so that it is always present
	logLevel.Set(appConfig.LogLevel)

	if !appConfig.InCluster {
		panic("running without in-cluster config is not implemented")
	}

	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		panic(err.Error())
	}

	cu, err := k8swatcher.CreateCgroupUpdater(ctx, clientset, *appConfig)
	if err != nil {
		panic(err.Error())
	}

	containerUpdates := make(chan string)
	defer close(containerUpdates)
	if appConfig.WatchContainerEvents {
		go cu.ContainerdHelper.WatchEvents(ctx, containerUpdates)
	}

	err = cu.Watch(ctx, containerUpdates)
	select {
	case <-ctx.Done():
		logger.Info("graceful shutdown finished")
	default:
		panic(err.Error())
	}
}
