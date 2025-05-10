package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"meoe.io/cgroup-burst/internal/appconfig"
	"meoe.io/cgroup-burst/internal/appmetrics"
	"meoe.io/cgroup-burst/internal/containerdhelper"
	"meoe.io/cgroup-burst/internal/k8swatcher"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	slogctx "github.com/veqryn/slog-context"
)

// When CPU limit is set to a low value,
// container initialization can be very slow.
// If own pod has an annotation for burst,
// we will eventually get burst config,
// but we need to wait until k8s watch sends own pod info,
// which may be the first one, or it may be the last one.
// This would make startup latency unpredictable,
// and potentially very high on busy clusters.
// As a workaround, we specifically set burst
// for our own container on startup, before doing anything.
// The startup latency is predictable and it's even slightly lower
// than lowest possible latency in the best case with k8s watch that returns current pod as the first.
func setStartupBurst(ctx context.Context, appConfig *appconfig.AppConfig) {
	ch, err := containerdhelper.CreateContainerdHandle(appConfig.ContainerdSocket, appConfig.SkipSameSpec, nil, "", "")
	if err != nil {
		panic(err)
	}
	defer ch.Close()

	// Usually when we change burst on container, we emit k8s event for this
	// but here we bypass k8s connection, and can't create events.
	// If burst annotation on the pod matches startup burst duration, we will skip emitting event for the initial change.
	// By doing '+ 1e-4' I try to set a value that will never be encountered in the burst annotation
	// so whatever burst is requested by annotation, an event is emitted.
	startupBurstSeconds := 1.0 + 1e-4
	err = ch.UpdateContainerByName(ctx, appConfig.PodName, appConfig.PodNamespace, startupBurstSeconds)
	if err != nil {
		panic(err)
	}
}

// setStartupBurst searches local pods on the node.
// Ss an alternative, you can use k8s client to get current pod info.
// The result is the same, but this requires us to establish a connection to k8s.
// This results in higher startup latency compared to setStartupBurst.
func setStartupBurstViaK8s(ctx context.Context, clientset *kubernetes.Clientset, appConfig *appconfig.AppConfig, cu *k8swatcher.CgroupUpdater) {
	ownPod, err := clientset.CoreV1().Pods(appConfig.PodNamespace).Get(ctx, appConfig.PodName, metav1.GetOptions{})
	if err != nil {
		panic(err.Error())
	}
	_, err = cu.UpdatePod(ctx, ownPod)
	if err != nil {
		panic(err.Error())
	}
}

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

	// run setStartupBurst as soon as logger is available
	setStartupBurst(ctx, appConfig)

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
	containerRegistry := prometheus.NewRegistry()

	ownMetrics := appmetrics.NewOwnMetrics(ownRegistry)
	containerMetrics := appmetrics.NewContainerMetrics(containerRegistry)

	cu, err := k8swatcher.CreateCgroupUpdater(ctx, clientset, *appConfig, ownMetrics, containerMetrics)
	if err != nil {
		panic(err.Error())
	}
	defer cu.Close()

	// run setStartupBurstViaK8s as soon as CgroupUpdater is available
	setStartupBurstViaK8s(ctx, clientset, appConfig, cu)

	containerUpdates := make(chan string)
	defer close(containerUpdates)
	if appConfig.WatchContainerEvents {
		go cu.ContainerdHelper.WatchEvents(ctx, containerUpdates)
	}

	ownMetricsHandler := promhttp.HandlerFor(ownRegistry, promhttp.HandlerOpts{
		ErrorLog: slog.NewLogLogger(logHandler, slog.LevelError),
	})
	containerMetricsHandler := promhttp.HandlerFor(containerRegistry, promhttp.HandlerOpts{
		ErrorLog: slog.NewLogLogger(logHandler, slog.LevelError),
	})
	if appConfig.EnableCgroupMetrics {
		logger.Info("adding cgroup gathering before /container_metrics endpoint")
		containerMetricsHandler = appmetrics.NewInterceptHandler(ctx, containerMetricsHandler, cu.GatherCgroupBurst, appConfig.CgroupUpdateDelay, appConfig.CgroupMetricsTimeout)
	}
	appmetrics.SetupMetricListener(ctx, appConfig.MetricsAddress, ownMetricsHandler, containerMetricsHandler, logHandler)

	err = cu.Watch(ctx, containerUpdates)
	if ctx.Err() != nil {
		logger.Info("graceful shutdown finished")
		return
	}
	if err != nil {
		panic(err.Error())
	}
}
