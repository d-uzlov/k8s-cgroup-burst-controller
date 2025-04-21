package appmetrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// descriptions
const (
	k8sWatchDescription        = "Number of k8s watch events, split by result type"
	podUpdateDescription       = "Pod updates, split by result type"
	containerUpdateDescription = "Container updates, split by result type"
)

const (
	ownPrometheusNamespace = "cgroup_burst"
)

type OwnMetrics struct {
	// general metrics
	K8sWatchStreamsTotal   prometheus.Counter
	K8sWatchBookmarksTotal prometheus.Counter
	K8sWatchUpdatesTotal   prometheus.Counter
	K8sWatchDeletesTotal   prometheus.Counter
	K8sWatchErrorsTotal    prometheus.Counter
	ContainerdEventsTotal  prometheus.Counter

	// pod metrics
	PodUpdatesK8sSuccessTotal        prometheus.Counter
	PodUpdatesK8sFailTotal           prometheus.Counter
	PodUpdatesK8sSkipTotal           prometheus.Counter
	PodUpdatesContainerdSuccessTotal prometheus.Counter
	// this one does not exist because the app panics on such events
	// PodUpdatesContainerdFailTotal prometheus.Counter
	PodUpdatesContainerdSkipTotal prometheus.Counter
	PodUnusedAnnotationsTotal     prometheus.Counter
	PodMissingAnnotationsTotal    prometheus.Counter

	// container metrics
	ContainerUpdateAttemptsSkipTotal    prometheus.Counter
	ContainerUpdateAttemptsFailTotal    prometheus.Counter
	ContainerUpdateAttemptsSuccessTotal prometheus.Counter
	ContainerIdCacheSize                prometheus.Gauge
}

func NewOwnMetrics(registry *prometheus.Registry) *OwnMetrics {
	prometheusNamespace := ownPrometheusNamespace

	// general metrics
	result := &OwnMetrics{
		K8sWatchStreamsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "k8s_watch",
			Name:      "streams_total",
			Help:      "Number of k8s watches",
		}),
		K8sWatchBookmarksTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "k8s_watch",
			Name:      "events_total",
			Help:      k8sWatchDescription,
			ConstLabels: prometheus.Labels{
				"type": "bookmark",
			},
		}),
		K8sWatchUpdatesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "k8s_watch",
			Name:      "events_total",
			Help:      k8sWatchDescription,
			ConstLabels: prometheus.Labels{
				"type": "update",
			},
		}),
		K8sWatchDeletesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "k8s_watch",
			Name:      "events_total",
			Help:      k8sWatchDescription,
			ConstLabels: prometheus.Labels{
				"type": "delete",
			},
		}),
		K8sWatchErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "k8s_watch",
			Name:      "events_total",
			Help:      k8sWatchDescription,
			ConstLabels: prometheus.Labels{
				"type": "error",
			},
		}),
		ContainerdEventsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "containerd",
			Name:      "events_total",
			Help:      "Number of events received from containerd watches",
		}),
		// pod metrics
		PodUpdatesK8sSuccessTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "pod",
			Name:      "updates_total",
			Help:      podUpdateDescription,
			ConstLabels: prometheus.Labels{
				"type": "success",
				"from": "k8s_event",
			},
		}),
		PodUpdatesK8sFailTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "pod",
			Name:      "updates_total",
			Help:      podUpdateDescription,
			ConstLabels: prometheus.Labels{
				"type": "fail",
				"from": "k8s_event",
			},
		}),
		PodUpdatesK8sSkipTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "pod",
			Name:      "updates_total",
			Help:      podUpdateDescription,
			ConstLabels: prometheus.Labels{
				"type": "skip",
				"from": "k8s_event",
			},
		}),
		PodUpdatesContainerdSuccessTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "pod",
			Name:      "updates_total",
			Help:      podUpdateDescription,
			ConstLabels: prometheus.Labels{
				"type": "success",
				"from": "containerd",
			},
		}),
		PodUpdatesContainerdSkipTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "pod",
			Name:      "updates_total",
			Help:      podUpdateDescription,
			ConstLabels: prometheus.Labels{
				"type": "skip",
				"from": "containerd",
			},
		}),
		PodUnusedAnnotationsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "pod",
			Name:      "annotations_unused_total",
			Help:      "Amount of annotations that specify containers that don't exist in the pod",
		}),
		PodMissingAnnotationsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "pod",
			Name:      "annotations_missing_total",
			Help:      "Amount of pods that match label but do not have an annotation",
		}),
		// container metrics
		ContainerUpdateAttemptsSkipTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "container",
			Name:      "updates_total",
			Help:      containerUpdateDescription,
			ConstLabels: prometheus.Labels{
				"type": "skip",
			},
		}),
		ContainerUpdateAttemptsFailTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "container",
			Name:      "updates_total",
			Help:      containerUpdateDescription,
			ConstLabels: prometheus.Labels{
				"type": "fail",
			},
		}),
		ContainerUpdateAttemptsSuccessTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: prometheusNamespace,
			Subsystem: "container",
			Name:      "updates_total",
			Help:      containerUpdateDescription,
			ConstLabels: prometheus.Labels{
				"type": "success",
			},
		}),
		ContainerIdCacheSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "id_cache_size",
			Help:      "Amount of entries in the container_id to pod_info map",
		}),
	}

	// general metrics
	registry.MustRegister(result.K8sWatchStreamsTotal)
	registry.MustRegister(result.K8sWatchBookmarksTotal)
	registry.MustRegister(result.K8sWatchUpdatesTotal)
	registry.MustRegister(result.K8sWatchDeletesTotal)
	registry.MustRegister(result.K8sWatchErrorsTotal)
	registry.MustRegister(result.ContainerdEventsTotal)

	// pod metrics
	registry.MustRegister(result.PodUpdatesK8sSuccessTotal)
	registry.MustRegister(result.PodUpdatesK8sFailTotal)
	registry.MustRegister(result.PodUpdatesK8sSkipTotal)
	registry.MustRegister(result.PodUpdatesContainerdSuccessTotal)
	registry.MustRegister(result.PodUpdatesContainerdSkipTotal)
	registry.MustRegister(result.PodUnusedAnnotationsTotal)
	registry.MustRegister(result.PodMissingAnnotationsTotal)

	// container metrics
	registry.MustRegister(result.ContainerUpdateAttemptsSkipTotal)
	registry.MustRegister(result.ContainerUpdateAttemptsFailTotal)
	registry.MustRegister(result.ContainerUpdateAttemptsSuccessTotal)
	registry.MustRegister(result.ContainerIdCacheSize)

	return result
}
