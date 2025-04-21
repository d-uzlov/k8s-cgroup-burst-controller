package containerdhelper

import (
	"context"
	"fmt"
	"time"

	"k8s.io/utils/ptr"
	"meoe.io/cgroup-burst/internal/appmetrics"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/pkg/errors"
	slogctx "github.com/veqryn/slog-context"
)

type ContainerdHelper struct {
	client           *containerd.Client
	containerService containers.Store
	eventsService    containerd.EventService
	skipSameSpec     bool
	ownMetrics       *appmetrics.OwnMetrics
	cgroupRoot       string
	procRoot         string
}

func (h *ContainerdHelper) Close() {
	h.client.Close()
}

func CreateContainerdHandle(socket string, skipSameSpec bool, ownMetrics *appmetrics.OwnMetrics, cgroupRoot string, procRoot string) (*ContainerdHelper, error) {
	// all pods created by k8s use k8s.io namespace in containerd
	client, err := containerd.New(socket, containerd.WithDefaultNamespace("k8s.io"))
	if err != nil {
		return nil, err
	}
	containerService := client.ContainerService()

	eventsService := client.EventService()

	return &ContainerdHelper{
		client:           client,
		containerService: containerService,
		eventsService:    eventsService,
		skipSameSpec:     skipSameSpec,
		ownMetrics:       ownMetrics,
		cgroupRoot:       cgroupRoot,
		procRoot:         procRoot,
	}, nil
}

func (h *ContainerdHelper) WatchEvents(ctx context.Context, containerUpdates chan<- string) {
	logger := slogctx.FromCtx(ctx)

	filters := []string{
		`topic=="/containers/update"`,
	}
	eventCh, errCh := h.eventsService.Subscribe(ctx, filters...)
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-eventCh:
			if !ok {
				if ctx.Err() != nil {
					return
				}
				panic("containerd event channel unexpectedly closed")
			}
			h.ownMetrics.ContainerdEventsTotal.Inc()
			event, err := typeurl.UnmarshalAny(evt.Event)
			if err != nil {
				panic(err)
			}
			switch e := event.(type) {
			case *events.ContainerUpdate:
				logger.Debug("containerd update event", "id", e.ID)
				select {
				case containerUpdates <- e.ID:
				case <-ctx.Done():
					return
				}
			case *events.ContainerCreate:
				panic("unexpected containerd create event")
			case *events.ContainerDelete:
				panic("unexpected containerd delete event")
			default:
				logger.Debug("Unhandled event type", "event", e)
				panic("containerd unhandled event type")
			}
		case err, ok := <-errCh:
			if !ok {
				if ctx.Err() != nil {
					return
				}
				panic("containerd error channel unexpectedly closed")
			}
			if err == nil {
				continue
			}
			if errdefs.IsCanceled(err) {
				return
			}
			logger.Error("Event error", "error", err.Error())
		}
	}
}

func (h *ContainerdHelper) UpdateContainer(ctx context.Context, id string, burstSeconds float64) (changed bool, err error) {
	logger := slogctx.FromCtx(ctx)

	burstTime := time.Duration(float64(time.Second) * burstSeconds)

	ctr, err := h.client.LoadContainer(ctx, id)
	if err != nil {
		return
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		return
	}
	spec, err := task.Spec(ctx)
	if err != nil {
		return
	}

	burstMks := uint64(burstTime.Microseconds())
	if h.skipSameSpec && spec.Linux.Resources.CPU.Burst != nil && *spec.Linux.Resources.CPU.Burst == burstMks {
		logger.Debug("skipping container update: old spec value matches new one")
		return
	}
	spec.Linux.Resources.CPU.Burst = ptr.To(burstMks)

	err = task.Update(ctx, containerd.WithResources(spec.Linux.Resources))
	if err != nil {
		return
	}
	changed = true
	logger.Debug("task update successful")

	updatedSpec, err := typeurl.MarshalAny(spec)
	if err != nil {
		return
	}

	_, err = h.containerService.Update(ctx, containers.Container{
		ID:   task.ID(),
		Spec: updatedSpec,
	}, "spec")
	if err != nil {
		return
	}
	logger.Debug("spec update successful")

	return
}

func (h *ContainerdHelper) UpdateContainerByName(ctx context.Context, podName string, podNamespace string, burstSeconds float64) (err error) {
	logger := slogctx.FromCtx(ctx).With("source", "UpdateContainerByName", "pod", podName, "namespace", podNamespace)

	filter := fmt.Sprintf(
		`labels."io.kubernetes.pod.name"=="%s",labels."io.kubernetes.pod.namespace"=="%s"`,
		podName,
		podNamespace,
	)
	containers, err := h.client.Containers(ctx, filter)
	if err != nil {
		return errors.Wrap(err, "failed to list containers")
	}

	if len(containers) == 0 {
		return fmt.Errorf("pod not found")
	}

	for _, container := range containers {
		id := container.ID()
		logger.Debug("found matching container", "container-id", id)
		_, err = h.UpdateContainer(ctx, id, burstSeconds)
		if err != nil {
			logger.Error("could not update container", "container-id", id, "error", err.Error())
		}
	}
	return nil
}

type CgroupBurstReader func() (nrBurst, burstSeconds float64, err error)

func (h *ContainerdHelper) GetCgroupBurstReader(ctx context.Context, id string) (CgroupBurstReader, error) {
	ctr, err := h.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		return nil, err
	}
	pid := int(task.Pid())

	slogctx.FromCtx(ctx).Debug("creating cgroup reader", "container-id", id, "pid", pid)

	result := func() (nrBurst float64, burstSeconds float64, err error) {
		return GetCPUMetrics(h.cgroupRoot, h.procRoot, pid)
	}
	return result, nil
}
