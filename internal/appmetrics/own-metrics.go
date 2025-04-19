package appmetrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	prometheusNamespace = "cgroup_burst"
)

// descriptions
const (
	k8sWatchDescription        = "Number of k8s watch events, split by result type"
	podUpdateDescription       = "Pod updates, split by result type"
	containerUpdateDescription = "Container updates, split by result type"
)

// general metrics
var (
	K8sWatchStreamsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "k8s_watch",
		Name:      "streams_total",
		Help:      "Number of k8s watches",
	})
	K8sWatchBookmarksTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "k8s_watch",
		Name:      "events_total",
		Help:      k8sWatchDescription,
		ConstLabels: prometheus.Labels{
			"type": "bookmark",
		},
	})
	K8sWatchUpdatesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "k8s_watch",
		Name:      "events_total",
		Help:      k8sWatchDescription,
		ConstLabels: prometheus.Labels{
			"type": "update",
		},
	})
	K8sWatchDeletesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "k8s_watch",
		Name:      "events_total",
		Help:      k8sWatchDescription,
		ConstLabels: prometheus.Labels{
			"type": "delete",
		},
	})
	K8sWatchErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "k8s_watch",
		Name:      "events_total",
		Help:      k8sWatchDescription,
		ConstLabels: prometheus.Labels{
			"type": "error",
		},
	})
	ContainerdEventsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "containerd",
		Name:      "events_total",
		Help:      "Number of events received from containerd watches",
	})
)

// pod metrics
var (
	PodUpdatesK8sSuccessTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "pod",
		Name:      "updates_total",
		Help:      podUpdateDescription,
		ConstLabels: prometheus.Labels{
			"type": "success",
			"from": "k8s_event",
		},
	})
	PodUpdatesK8sFailTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "pod",
		Name:      "updates_total",
		Help:      podUpdateDescription,
		ConstLabels: prometheus.Labels{
			"type": "fail",
			"from": "k8s_event",
		},
	})
	PodUpdatesK8sSkipTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "pod",
		Name:      "updates_total",
		Help:      podUpdateDescription,
		ConstLabels: prometheus.Labels{
			"type": "skip",
			"from": "k8s_event",
		},
	})
	PodUpdatesContainerdSuccessTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "pod",
		Name:      "updates_total",
		Help:      podUpdateDescription,
		ConstLabels: prometheus.Labels{
			"type": "success",
			"from": "containerd",
		},
	})
	// PodUpdatesContainerdFailTotal
	// this one does not exist because the app panics on such events
	PodUpdatesContainerdSkipTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "pod",
		Name:      "updates_total",
		Help:      podUpdateDescription,
		ConstLabels: prometheus.Labels{
			"type": "skip",
			"from": "containerd",
		},
	})
	PodUnusedAnnotationsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "pod",
		Name:      "annotations_unused_total",
		Help:      "Amount of annotations that specify containers that don't exist in the pod",
	})
	PodMissingAnnotationsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "pod",
		Name:      "annotations_missing_total",
		Help:      "Amount of pods that match label but do not have an annotation",
	})
)

// container metrics
var (
	ContainerUpdateAttemptsSkipTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "container",
		Name:      "updates_total",
		Help:      containerUpdateDescription,
		ConstLabels: prometheus.Labels{
			"type": "skip",
		},
	})
	ContainerUpdateAttemptsFailTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "container",
		Name:      "updates_total",
		Help:      containerUpdateDescription,
		ConstLabels: prometheus.Labels{
			"type": "fail",
		},
	})
	ContainerUpdateAttemptsSuccessTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: prometheusNamespace,
		Subsystem: "container",
		Name:      "updates_total",
		Help:      containerUpdateDescription,
		ConstLabels: prometheus.Labels{
			"type": "success",
		},
	})
	ContainerIdCacheSize = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: prometheusNamespace,
		Name:      "id_cache_size",
		Help:      "Amount of entries in the container_id to pod_info map",
	})
)
