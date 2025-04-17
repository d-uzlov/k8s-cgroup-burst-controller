package containerdhelper

import (
	"context"
	"time"

	"k8s.io/utils/ptr"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/dustin/go-humanize"
	slogctx "github.com/veqryn/slog-context"
)

type ContainerdHelper struct {
	client           *containerd.Client
	containerService containers.Store
	eventsService    containerd.EventService
	skipSameSpec     bool
}

func CreateContainerdHandle(socket string, skipSameSpec bool) (*ContainerdHelper, error) {
	// all pods created by k8s use k8s.io namespace in containerd
	client, err := containerd.New(socket, containerd.WithDefaultNamespace("k8s.io"))
	if err != nil {
		return nil, err
	}
	containerService := client.ContainerService()

	// Create event service client
	eventsService := client.EventService()

	return &ContainerdHelper{
		client:           client,
		containerService: containerService,
		eventsService:    eventsService,
		skipSameSpec:     skipSameSpec,
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
		case evt := <-eventCh:
			// logger.Info("got event from containerd", "event", evt)
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
		case err := <-errCh:
			if err == nil {
				return
			}
			if errdefs.IsCanceled(err) {
				return
			}
			logger.Error("Event error", "error", err.Error())
		}
	}
}

func (h *ContainerdHelper) UpdateContainer(ctx context.Context, id string, containerBurst string) (changed bool, err error) {
	logger := slogctx.FromCtx(ctx)

	burstF, _, err := humanize.ParseSI(containerBurst)
	if err != nil {
		return
	}
	burstTime := time.Duration(float64(time.Second) * burstF)

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
