package appconfig

import (
	"flag"
	"log/slog"
	"time"

	"github.com/kouhin/envflag"
)

type AppConfig struct {
	LogLevel                slog.Level
	NodeName                string
	PodName                 string
	PodNamespace            string
	InCluster               bool
	WatchTimeout            time.Duration
	Hostname                string
	ContainerdSocket        string
	LabelSelector           string
	BurstAnnotation         string
	SkipSameSpec            bool
	WatchContainerEvents    bool
	OwnMetricsAddress       string
	ContainerMetricsAddress string
}

func ParseConfig() *AppConfig {
	result := &AppConfig{}

	flag.StringVar(&result.NodeName, "node-name", "", "Required: k8s node name to use as a watch filter")
	flag.StringVar(&result.PodName, "pod-name", "", "Required: name of the pod running the app")
	flag.StringVar(&result.PodNamespace, "pod-namespace", "", "Required: namespace of the pod running the app")
	flag.BoolVar(&result.InCluster, "use-in-cluster-config", true, "Try to use in-cluster service account configuration")
	flag.DurationVar(&result.WatchTimeout, "watch-timeout", time.Hour, "Server timeout for k8s client watch calls")
	flag.StringVar(&result.Hostname, "hostname", "", "Hostname to use in logs, if it needs to be different from OS-provided value")
	flag.StringVar(&result.ContainerdSocket, "containerd-socket", "/run/containerd/containerd.sock", "Socket to connect to in the group-burst filesystem")
	flag.StringVar(&result.LabelSelector, "label-selector", "cgroup.meoe.io/burst=enable", "Filter pods on the node using this selector")
	flag.StringVar(&result.BurstAnnotation, "burst-annotation", "cgroup.meoe.io/burst", "Name of the annotation to parse burst config")
	var logLevel string
	flag.StringVar(&logLevel, "log-level", "info", "One of: error, warn, info, debug")
	flag.BoolVar(&result.SkipSameSpec, "skip-same-spec", true, "Watcher sometimes receives repeated updates on pods. Usually you can skip updating container on these repeated events")
	flag.BoolVar(&result.WatchContainerEvents, "watch-container-events", true, "Runtime container spec can sometimes be updated without changes in pod")
	flag.StringVar(&result.OwnMetricsAddress, "own-metrics-address", ":2112", "Address to listen on for own app metrics")
	flag.StringVar(&result.ContainerMetricsAddress, "container-metrics-address", ":2113", "Address to listen on for container metrics")

	if err := envflag.Parse(); err != nil {
		panic(err)
	}

	if result.NodeName == "" {
		panic("Node Name is missing")
	}
	if result.Hostname == "" {
		panic("Hostname is missing")
	}
	if result.PodName == "" {
		panic("Pod Name is missing")
	}
	if result.PodNamespace == "" {
		panic("Pod Namespace is missing")
	}

	switch logLevel {
	case "error":
		result.LogLevel = slog.LevelError
	case "warn":
		result.LogLevel = slog.LevelWarn
	case "info":
		result.LogLevel = slog.LevelInfo
	case "debug":
		result.LogLevel = slog.LevelDebug
	default:
		panic("unknown log level: " + logLevel)
	}

	if !result.SkipSameSpec && result.WatchContainerEvents {
		panic("watch-container-events=true requires skip-same-spec=true")
	}

	return result
}
