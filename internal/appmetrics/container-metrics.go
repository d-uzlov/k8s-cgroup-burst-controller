package appmetrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

type ContainerMetrics struct {
	SpecCgroupBurst    *prometheus.GaugeVec
	CgroupBurstNr      *prometheus.CounterVec
	CgroupBurstSeconds *prometheus.CounterVec
}

func NewContainerMetrics(registry *prometheus.Registry) *ContainerMetrics {
	result := &ContainerMetrics{
		SpecCgroupBurst: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kube_pod_container_cgroup_burst",
			Help: "Specified burst duration in seconds",
		}, []string{"node", "namespace", "pod", "container"}),
		CgroupBurstNr: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "container_cpu_cgroup_burst_periods_total",
			Help: "Number of periods burst was used (nr_bursts)",
		}, []string{"node", "namespace", "pod", "container"}),
		CgroupBurstSeconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "container_cpu_cgroup_burst_seconds_total",
			Help: "Amount of time burst was used (burst_usec)",
		}, []string{"node", "namespace", "pod", "container"}),
	}

	registry.Register(result.SpecCgroupBurst)
	registry.Register(result.CgroupBurstNr)
	registry.Register(result.CgroupBurstSeconds)
	return result
}
