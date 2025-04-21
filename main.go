package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"meoe.io/cgroup-burst/internal/appconfig"
	"meoe.io/cgroup-burst/internal/appmetrics"
	"meoe.io/cgroup-burst/internal/k8swatcher"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	slogctx "github.com/veqryn/slog-context"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	appConfig := appconfig.ParseConfig()

	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	logHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})
	logger := slog.New(logHandler)

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

	ownRegistry := prometheus.NewRegistry()

	ownMetrics := appmetrics.NewOwnMetrics(ownRegistry)

	cu, err := k8swatcher.CreateCgroupUpdater(ctx, clientset, *appConfig, ownMetrics)
	if err != nil {
		panic(err.Error())
	}

	containerUpdates := make(chan string)
	defer close(containerUpdates)
	if appConfig.WatchContainerEvents {
		go cu.ContainerdHelper.WatchEvents(ctx, containerUpdates)
	}

	http.Handle("/metrics", promhttp.HandlerFor(ownRegistry, promhttp.HandlerOpts{
		ErrorLog: slog.NewLogLogger(logHandler, slog.LevelError),
	}))
	metricsAddress := ":2112"
	go func() {
		logger.Info("started metrics listener", "address", metricsAddress)
		err := http.ListenAndServe(metricsAddress, nil)
		if ctx.Err() != nil {
			return
		}
		panic(err.Error())
	}()

	err = cu.Watch(ctx, containerUpdates)
	if ctx.Err() != nil {
		logger.Info("graceful shutdown finished")
		return
	}
	panic(err.Error())
}
