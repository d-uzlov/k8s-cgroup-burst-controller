package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kouhin/envflag"
	slogctx "github.com/veqryn/slog-context"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	appConfig := parseConfig()

	logger = logger.With("host", appConfig.Hostname)
	ctx = slogctx.NewCtx(ctx, logger)

	logger.Info("using config", "config", appConfig)
	_ = appConfig

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

	lu, err := createLimitUpdater(ctx, clientset, *appConfig)
	if err != nil {
		panic(err.Error())
	}
	for err == nil {
		err = lu.runAndWatch(ctx)
	}
	select {
	case <-ctx.Done():
		logger.Info("graceful shutdown finished")
	default:
		panic(err.Error())
	}
}

type AppConfig struct {
	NodeName         string
	InCluster        bool
	WatchTimeout     time.Duration
	Hostname         string
	ContainerdSocket string
	LabelSelector    string
	BurstAnnotation  string
}

func parseConfig() *AppConfig {
	result := &AppConfig{}

	flag.StringVar(&result.NodeName, "node-name", "", "Required: k8s node name to use as a watch filter")
	flag.BoolVar(&result.InCluster, "use-in-cluster-config", true, "Try to use in-cluster service account configuration")
	flag.DurationVar(&result.WatchTimeout, "watch-timeout", time.Hour, "Server timeout for k8s client watch calls")
	flag.StringVar(&result.Hostname, "hostname", "", "Hostname to use in logs, if it needs to be different from OS-provided value")
	flag.StringVar(&result.ContainerdSocket, "containerd-socket", "/run/containerd/containerd.sock", "Socket to connect to in the group-burst filesystem")
	flag.StringVar(&result.LabelSelector, "label-selector", "cgroup.meoe.io/burst=enable", "Filter pods on the node using this selector")
	flag.StringVar(&result.BurstAnnotation, "burst-annotation", "cgroup.meoe.io/burst", "Name of the annotation to parse burst config")

	if err := envflag.Parse(); err != nil {
		panic(err)
	}

	if result.NodeName == "" {
		panic("Node Name is missing")
	}

	if result.Hostname == "" {
		panic("Hostname is missing")
	}

	return result
}
