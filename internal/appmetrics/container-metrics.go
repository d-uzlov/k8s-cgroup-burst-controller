package appmetrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

type ContainerMetrics struct {
	SpecCgroupBurst    *prometheus.GaugeVec
	// these metrics are calculated in the linux kernel
	// prometheus insists that counters should not have Set() method
	// Using the gauge for a counter metric is a bit hacky
	// but I don't think there are any downsides, except documentation mismatch
	CgroupBurstNr      *prometheus.GaugeVec
	CgroupBurstSeconds *prometheus.GaugeVec
}

func NewContainerMetrics(registry *prometheus.Registry) *ContainerMetrics {
	result := &ContainerMetrics{
		SpecCgroupBurst: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kube_pod_container_cgroup_burst",
			Help: "Specified burst duration in seconds",
		}, []string{"node", "namespace", "pod", "container"}),
		CgroupBurstNr: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "container_cpu_cgroup_burst_periods_total",
			Help: "Number of periods burst was used (nr_bursts)",
		}, []string{"node", "namespace", "pod", "container", "name"}),
		CgroupBurstSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "container_cpu_cgroup_burst_seconds_total",
			Help: "Amount of time burst was used (burst_usec)",
		}, []string{"node", "namespace", "pod", "container", "name"}),
	}

	registry.MustRegister(result.SpecCgroupBurst)
	registry.MustRegister(result.CgroupBurstNr)
	registry.MustRegister(result.CgroupBurstSeconds)
	return result
}
