package appconfig

import (
	"flag"
	"log/slog"
	"time"

	"github.com/kouhin/envflag"
)

type AppConfig struct {
	LogLevel             slog.Level
	NodeName             string
	PodName              string
	PodNamespace         string
	InCluster            bool
	WatchTimeout         time.Duration
	Hostname             string
	ContainerdSocket     string
	LabelSelector        string
	BurstAnnotation      string
	SkipSameSpec         bool
	WatchContainerEvents bool
	MetricsAddress       string
	CgroupRoot           string
	ProcRoot             string
	CgroupUpdateDelay    time.Duration
	CgroupMetricsTimeout time.Duration
	CgroupPathAlgorithm  string
}

const (
	CgroupFromNone    = "none"
	CgroupFromPid     = "pid"
	CgroupFromSpec    = "spec"
	CgroupFromSpecPid = "spec-pid"
)

func ParseConfig() *AppConfig {
	result := &AppConfig{}

	flag.StringVar(&result.NodeName, "node-name", "", "[Required] K8s node name to use as a watch filter")
	flag.StringVar(&result.PodName, "pod-name", "", "[Required] Name of the pod running the app")
	flag.StringVar(&result.PodNamespace, "pod-namespace", "", "[Required] Namespace of the pod running the app")
	flag.BoolVar(&result.InCluster, "use-in-cluster-config", true, "Try to use in-cluster service account configuration")
	flag.DurationVar(&result.WatchTimeout, "watch-timeout", time.Hour, "Server timeout for k8s client watch calls")
	flag.StringVar(&result.Hostname, "hostname", "", "Hostname to use in logs, if it needs to be different from OS-provided value")
	flag.StringVar(&result.ContainerdSocket, "containerd-socket", "/run/containerd/containerd.sock", "Socket to connect to in the container filesystem")
	flag.StringVar(&result.LabelSelector, "label-selector", "cgroup.meoe.io/burst=enable", "Filter pods on the node using this selector")
	flag.StringVar(&result.BurstAnnotation, "burst-annotation", "cgroup.meoe.io/burst", "Name of the annotation to parse burst config")
	var logLevel string
	flag.StringVar(&logLevel, "log-level", "info", "One of: error, warn, info, debug")
	flag.BoolVar(&result.SkipSameSpec, "skip-same-spec", true, "Watcher sometimes receives repeated updates on pods. Usually you can skip updating container on these repeated events")
	flag.BoolVar(&result.WatchContainerEvents, "watch-container-events", true, "Watch events from container runtime, in case container spec changed without changes in pod")
	flag.StringVar(&result.MetricsAddress, "metrics-address", ":2112", "Address to listen on for metrics")
	flag.StringVar(&result.CgroupRoot, "cgroup-root", "/sys/fs/cgroup", "Path to the root of the cgroup mount with host data")
	flag.StringVar(&result.ProcRoot, "proc-root", "/proc", "Path to the root of the /proc mount with host data")
	flag.DurationVar(&result.CgroupUpdateDelay, "cgroup-update-delay", time.Second*10, "Cgroup metrics can be outdated, this parameter limits time between gathering cgroup metrics")
	flag.DurationVar(&result.CgroupMetricsTimeout, "cgroup-metrics-timeout", time.Second, "Timeout for updating cgroup metrics")
	flag.StringVar(&result.CgroupPathAlgorithm, "cgroup-path-algorithm", "none",
		`One of: none, pid, spec, spec-pid.
When 'none' is used containers metrics are generated only for spec values, runtime info is missing. Otherwise, you need to configure access to cgroup data.
pid: cgroup path is read from procfs. You need to have access to original unaltered procfs. This usually requires you to use privileged container.
spec: try to decode cgroup path from container spec. Relation between spec and real path is not stable, so it may break with updates.
spec-pid: use spec, fallback to pid when spec failed. Requires prerequisites from both types.`,
	)

	if err := envflag.Parse(); err != nil {
		panic(err)
	}

	if result.NodeName == "" {
		panic("Node Name is missing")
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

	switch result.CgroupPathAlgorithm {
	case CgroupFromNone:
	case CgroupFromPid:
	case CgroupFromSpec:
	case CgroupFromSpecPid:
	default:
		panic("invalid cgroup-path-algorithm: " + result.CgroupPathAlgorithm)
	}

	return result
}
