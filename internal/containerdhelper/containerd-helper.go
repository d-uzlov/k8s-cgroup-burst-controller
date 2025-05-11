package containerdhelper

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/utils/ptr"
	"meoe.io/cgroup-burst/internal/appconfig"
	"meoe.io/cgroup-burst/internal/appmetrics"

	"github.com/containerd/cgroups"
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
	burstUs := uint64(time.Duration(float64(time.Second) * burstSeconds).Microseconds())
	logger := slogctx.FromCtx(ctx).With("burst-us", burstUs)

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

	if h.skipSameSpec && spec.Linux.Resources.CPU.Burst != nil && *spec.Linux.Resources.CPU.Burst == burstUs {
		logger.Debug("skipping container update: old spec value matches new one")
		return
	}
	spec.Linux.Resources.CPU.Burst = ptr.To(burstUs)

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
	ctx = slogctx.With(ctx, "source", "UpdateContainerByName", "pod", podName, "namespace", podNamespace, "burst-seconds", burstSeconds)
	logger := slogctx.FromCtx(ctx)

	logger.Info("updating all containers with name filter")

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

func (h *ContainerdHelper) getCgroupFolderFromSpec(ctx context.Context, task containerd.Task) ([]string, error) {
	spec, err := task.Spec(ctx)
	if err != nil {
		return nil, err
	}

	slogctx.FromCtx(ctx).Debug("creating cgroup path from spec", "spec-cgroup-path", spec.Linux.CgroupsPath)

	// - spec format: `kubepods-burstable-podf6a32139_8482_42e2_9c9b_6269fed32a26.slice:cri-containerd:34c846d151d9e5faab4d5773c0c560abb318178b14defa5af5a5d0254cffacfc`
	// - format 1: `/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podf6a32139_8482_42e2_9c9b_6269fed32a26.slice/cri-containerd-34c846d151d9e5faab4d5773c0c560abb318178b14defa5af5a5d0254cffacfc.scope`
	// - format 2: `/kubepods/burstable/podf6a32139-8482-42e2-9c9b-6269fed32a26/34c846d151d9e5faab4d5773c0c560abb318178b14defa5af5a5d0254cffacfc`
	// - format 3: `/system.slice/containerd.service/kubepods-burstable-podf6a32139_8482_42e2_9c9b_6269fed32a26.slice/cri-containerd-34c846d151d9e5faab4d5773c0c560abb318178b14defa5af5a5d0254cffacfc`

	result := []string{}
	colonComponents := strings.Split(spec.Linux.CgroupsPath, ":")
	if len(colonComponents) != 3 {
		return nil, fmt.Errorf("expected two ':' values in string")
	}
	dashComponents := strings.Split(colonComponents[0], "-")
	option1 := ""
	for i := range dashComponents {
		currentComponent := dashComponents[0]
		for j := 1; j <= i; j++ {
			currentComponent += "-" + strings.TrimSuffix(dashComponents[j], ".slice")
		}
		option1 += "/" + currentComponent + ".slice"
	}
	option1 += "/" + colonComponents[1] + "-" + colonComponents[2] + ".scope"
	result = append(result, option1)

	option2 := ""
	for _, v := range dashComponents {
		v = strings.ReplaceAll(v, "_", "-")
		v = strings.TrimSuffix(v, ".slice")
		option2 += "/" + v
	}
	option2 += "/" + colonComponents[2]
	result = append(result, option2)

	option3 := "/system.slice/containerd.service/" + colonComponents[0] + "/" + colonComponents[1] + "-" + colonComponents[2]
	result = append(result, option3)

	return result, nil
}

func (h *ContainerdHelper) getCgroupFolderFromPid(task containerd.Task) ([]string, error) {
	pid := int(task.Pid())

	pidCgroupFile := fmt.Sprintf("%v/%v/cgroup", h.procRoot, pid)
	cgroupBytes, err := os.ReadFile(pidCgroupFile)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read %v", pidCgroupFile)
	}
	text := string(cgroupBytes)
	parts := strings.SplitN(text, ":", 3)
	if len(parts) < 3 {
		return nil, fmt.Errorf("invalid cgroup entry: %q", text)
	}
	if parts[0] != "0" || parts[1] != "" {
		return nil, fmt.Errorf("invalid cgroup entry: %q", text)
	}
	path := strings.TrimSuffix(parts[2], "\n")
	return []string{path}, nil
}

func (h *ContainerdHelper) GetCgroupBurstReader(ctx context.Context, id string, cgroupPathAlgorithm string) (appmetrics.CgroupUpdaterFunction, error) {
	logger := slogctx.FromCtx(ctx)

	if mode := cgroups.Mode(); mode != cgroups.Unified {
		// only cgroup v2 is supported
		return nil, fmt.Errorf("unknown cgroup mode: %v", mode)
	}

	ctr, err := h.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		return nil, err
	}

	var cgroupFolderList []string
	switch cgroupPathAlgorithm {
	case appconfig.CgroupFromPid:
		cgroupFolderList, err = h.getCgroupFolderFromPid(task)
		if err != nil {
			return nil, err
		}
	case appconfig.CgroupFromSpec, appconfig.CgroupFromSpecPid:
		cgroupFolderList, err = h.getCgroupFolderFromSpec(ctx, task)
		if err != nil {
			return nil, err
		}
	}

	cgroupFolder := ""
	for _, v := range cgroupFolderList {
		folder := h.cgroupRoot + v
		logger.Debug("checking cgroup folder", "folder", folder)
		if _, err := os.Stat(folder); os.IsNotExist(err) {
			continue
		}
		cgroupFolder = folder
		break
	}
	if cgroupFolder == "" {
		if cgroupPathAlgorithm == appconfig.CgroupFromSpecPid {
			cgroupFolderList, err = h.getCgroupFolderFromPid(task)
			if err != nil {
				return nil, err
			}
		}
		for _, v := range cgroupFolderList {
			folder := h.cgroupRoot + v
			if _, err := os.Stat(folder); os.IsNotExist(err) {
				continue
			}
			cgroupFolder = folder
			break
		}
	}
	if cgroupFolder == "" {
		return nil, fmt.Errorf("cgroup folder is not found")
	}

	logger.Debug("creating cgroup reader", "cgroup-folder", cgroupFolder)

	result := func() (nrBurst float64, burstSeconds float64, err error) {
		return GetCPUMetrics(cgroupFolder, h.procRoot)
	}
	return result, nil
}
