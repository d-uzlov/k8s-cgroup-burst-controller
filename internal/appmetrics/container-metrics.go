package appmetrics

import (
	"context"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	slogctx "github.com/veqryn/slog-context"
)

type ContainerMetrics struct {
	SpecCgroupBurst *prometheus.GaugeVec
	// these metrics are calculated in the linux kernel
	// prometheus insists that counters should not have Set() method
	// Using the gauge for a counter metric is a bit hacky
	// but I don't think there are any downsides, except documentation mismatch
	CgroupBurstNr      *prometheus.GaugeVec
	CgroupBurstSeconds *prometheus.GaugeVec

	requestChan   chan MetricOperation
	updaters      map[string]metricUpdater
	updatersMutex sync.Mutex
}

type metricUpdater struct {
	update CgroupUpdaterFunction
	labels prometheus.Labels
}

const (
	CacheAdd = iota
	CacheRemove
	CachePurge
)

type CgroupUpdaterFunction func() (nrBurst, burstSeconds float64, err error)

type MetricOperation struct {
	Operation int
	Id        string
	Update    CgroupUpdaterFunction
	Labels    prometheus.Labels
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
		// receiving from requestChan can block for relatively long time when update is blocking the mutex
		// let's make a small buffer to reduce probability of long send to this channel
		requestChan: make(chan MetricOperation, 10),
		updaters:    map[string]metricUpdater{},
	}

	registry.MustRegister(result.SpecCgroupBurst)
	registry.MustRegister(result.CgroupBurstNr)
	registry.MustRegister(result.CgroupBurstSeconds)
	return result
}

func (cm *ContainerMetrics) GetRequestChan() chan<- MetricOperation {
	return cm.requestChan
}

func (cm *ContainerMetrics) CloseRequestChan() {
	close(cm.requestChan)
}

func (cm *ContainerMetrics) RunOperations() {
	for {
		// If we were to check context and exit here, we could leave someone hanging on requestChan<-.
		// Instead, caller should call CloseRequestChan when requestChan is no longer used.
		op, ok := <-cm.requestChan
		if !ok {
			return
		}
		switch op.Operation {
		case CacheAdd:
			cm.handleAddOp(op)
		case CacheRemove:
			cm.handleRemoveOp(op)
		case CachePurge:
			cm.handlePurgeOp()
		default:
			panic("unknown operation type " + strconv.Itoa(op.Operation))
		}
	}
}

func (cm *ContainerMetrics) handleAddOp(op MetricOperation) {
	cm.updatersMutex.Lock()
	defer cm.updatersMutex.Unlock()

	cm.updaters[op.Id] = metricUpdater{
		update: op.Update,
		labels: op.Labels,
	}
}

func (cm *ContainerMetrics) handleRemoveOp(op MetricOperation) {
	updater, ok := cm.updaters[op.Id]
	if !ok {
		return
	}
	cm.updatersMutex.Lock()
	defer cm.updatersMutex.Unlock()

	cm.CgroupBurstNr.Delete(updater.labels)
	cm.CgroupBurstSeconds.Delete(updater.labels)
	delete(cm.updaters, op.Id)
}

func (cm *ContainerMetrics) handlePurgeOp() {
	cm.updatersMutex.Lock()
	defer cm.updatersMutex.Unlock()
	cm.updaters = map[string]metricUpdater{}
}

func (cm *ContainerMetrics) RunUpdates(ctx context.Context) error {
	logger := slogctx.FromCtx(ctx)
	cm.updatersMutex.Lock()
	defer cm.updatersMutex.Unlock()
	for id, v := range cm.updaters {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		nrBurst, burstSeconds, err := v.update()
		if err != nil {
			logger.Error("could not read cgroup metrics", "error", err.Error(), "labels", v.labels)
			// if we can't read cgroup metrics now, assume we will never be able to read them
			cm.GetRequestChan() <- MetricOperation{
				Operation: CacheRemove,
				Id:        id,
			}
			continue
		}
		cm.CgroupBurstNr.With(v.labels).Set(nrBurst)
		cm.CgroupBurstSeconds.With(v.labels).Set(burstSeconds)
	}
	return nil
}
