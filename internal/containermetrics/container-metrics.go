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
	K8sWatchStreamsTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: prometheusNamespace,
		Subsystem: "k8s_watch",
		Name:      "streams_total",
		Help:      "Number of k8s watches",
	}, []string{})
)
