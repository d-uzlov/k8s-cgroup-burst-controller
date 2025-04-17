package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"
	slogctx "github.com/veqryn/slog-context"
)

const (
	eventPodError       = "CgroupPodError"
	eventContainerError = "CgroupContainerError"
	eventContainerSet   = "CgroupBurstSet"
)

type limitUpdater struct {
	appConfig           AppConfig
	clientset           *kubernetes.Clientset
	ch                  *containerdHandle
	er                  record.EventRecorder
	lastResourceVersion string
	containerToPod      map[string]*corev1.Pod
}

func createLimitUpdater(ctx context.Context, clientset *kubernetes.Clientset, appConfig AppConfig) (*limitUpdater, error) {
	ch, err := createContainerdHandle(appConfig.ContainerdSocket, appConfig.SkipSameSpec)
	if err != nil {
		return nil, err
	}
	er := setupEventRecorder(ctx, clientset, appConfig.Hostname)
	return &limitUpdater{
		appConfig:           appConfig,
		clientset:           clientset,
		ch:                  ch,
		er:                  er,
		lastResourceVersion: "0",
		containerToPod:      map[string]*corev1.Pod{},
	}, nil
}

func (lu *limitUpdater) createWatcher(ctx context.Context) (watcher watch.Interface, err error) {
	logger := slogctx.FromCtx(ctx)

	logger.Info("starting new watch", "from-version", lu.lastResourceVersion)
	return lu.clientset.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{
		Watch:                true,
		SendInitialEvents:    ptr.To(true),
		AllowWatchBookmarks:  true,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		ResourceVersion:      lu.lastResourceVersion,
		TimeoutSeconds:       ptr.To(int64(lu.appConfig.WatchTimeout.Seconds())),
		FieldSelector:        "spec.nodeName=" + lu.appConfig.NodeName,
		LabelSelector:        lu.appConfig.LabelSelector,
	})
}

func (lu *limitUpdater) watch(ctx context.Context, containerUpdates <-chan string) error {
	watcher, err := lu.createWatcher(ctx)
	if err != nil {
		return err
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case podEvent, ok := <-watcher.ResultChan():
			if !ok {
				watcher.Stop()
				watcher, err = lu.createWatcher(ctx)
				if err != nil {
					return err
				}
				continue
			}
			err = lu.handlePodEvent(ctx, podEvent)
			if err != nil {
				return err
			}
		case id, ok := <-containerUpdates:
			if !ok {
				return fmt.Errorf("containerd updates channel is closed")
			}
			pod, ok := lu.containerToPod[id]
			if !ok {
				continue
			}

			callCtx := slogctx.With(ctx, "pod", pod.Name, "namespace", pod.Namespace, "update-type", "restore on containerd event")

			// the program should receive events of its own updates
			// this is fine, because on the second iteration
			// the program will see that there are no changes in burst spec and skip the update

			// we call the full handlePod method because in case we need to emit events, we need full pod context
			_, err = lu.handlePod(callCtx, pod)
			if err != nil {
				panic("previously this pod worked fine but we got an error when updating it on event: pod " + pod.Name + ": " + err.Error())
			}
		}
	}
}

func (lu *limitUpdater) handlePodEvent(ctx context.Context, event watch.Event) error {
	logger := slogctx.FromCtx(ctx)
	switch event.Type {
	case watch.Bookmark:
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			return nil
		}
		logger.Debug("bookmark received", "resource-version", pod.ResourceVersion)
		lu.lastResourceVersion = pod.ResourceVersion
	case watch.Added:
		err := lu.handlePodUpdate(ctx, event)
		if err != nil {
			return err
		}
	case watch.Modified:
		err := lu.handlePodUpdate(ctx, event)
		if err != nil {
			return err
		}
	case watch.Deleted:
	case watch.Error:
		status, ok := event.Object.(*metav1.Status)
		if !ok {
			return fmt.Errorf("unexpected error type %T", event.Object)
		}
		if status.Reason == metav1.StatusReasonTimeout {
			logger.Debug("watch timeout")
			return nil
		}
		return fmt.Errorf("received error status %v", status)
	default:
		return fmt.Errorf("unknown event type: %s", event.Type)
	}
	return nil
}

func (lu *limitUpdater) handlePodUpdate(ctx context.Context, event watch.Event) error {
	pod, ok := event.Object.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("event object is not a pod: %T", event.Object)
	}
	ctx = slogctx.With(ctx, "pod", pod.Name, "namespace", pod.Namespace)
	logger := slogctx.FromCtx(ctx)
	_, err := lu.handlePod(ctx, pod)
	if err != nil {
		// error here is not propagated intentionally
		logger.Error(err.Error())
		lu.er.Event(pod, corev1.EventTypeWarning, eventPodError, err.Error())
	}
	return nil
}

func parseMultiConfig(config string) (map[string]string, error) {
	components := strings.Split(config, ",")
	result := map[string]string{}
	for _, v := range components {
		kv := strings.Split(v, "=")
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid format: expected 'container-name=<value>': %v", v)
		}
		result[kv[0]] = kv[1]
	}
	return result, nil
}

func (lu *limitUpdater) handlePod(ctx context.Context, pod *corev1.Pod) (changed bool, err error) {
	logger := slogctx.FromCtx(ctx)
	pa := pod.Annotations
	if pa == nil {
		return false, fmt.Errorf("burst config is missing from annotations")
	}
	burstConfigRaw, ok := pa[lu.appConfig.BurstAnnotation]
	if !ok {
		return false, fmt.Errorf("burst config is missing from annotations")
	}
	burstConfig, err := parseMultiConfig(burstConfigRaw)
	if err != nil {
		return false, errors.Wrapf(err, "could not parse burst annotation")
	}
	logger.Debug("parsed burst annotation", "value", burstConfig)
	if len(burstConfig) == 0 {
		logger.Warn("burst annotation is empty")
		return false, nil
	}
	logger.Debug("found matching pod")
	for _, containerStatus := range pod.Status.ContainerStatuses {
		containerBurst, ok := burstConfig[containerStatus.Name]
		if !ok {
			// TODO try to remove burst from such containers
			continue
		}
		containerLogger := logger.With("container", containerStatus.Name)

		fullId := containerStatus.ContainerID
		if !strings.HasPrefix(fullId, "containerd://") {
			return false, fmt.Errorf("unexpected container ID prefix: %v", fullId)
		}
		rawId := strings.TrimPrefix(fullId, "containerd://")

		containerChanged, err := lu.ch.updateContainer(ctx, rawId, containerBurst)
		if err != nil {
			// error here is not propagated intentionally
			containerLogger.Error("failed to set burst", "value", containerBurst, "error", err.Error())
			lu.er.Event(pod, corev1.EventTypeWarning, eventContainerError, containerStatus.Name+": unable to set cpu burst: "+err.Error())
			continue
		}
		if containerChanged {
			changed = true
			containerLogger.Info("set burst", "value", containerBurst)
			lu.er.Event(pod, corev1.EventTypeNormal, eventContainerSet, containerStatus.Name+": set cpu burst to "+containerBurst)
		}
		containerLogger.Debug("saving container-to-pod mapping", "id", rawId)
		lu.containerToPod[rawId] = pod
	}

	return
}

type containerdHandle struct {
	client           *containerd.Client
	containerService containers.Store
	eventsService    containerd.EventService
	skipSameSpec     bool
}

func createContainerdHandle(socket string, skipSameSpec bool) (*containerdHandle, error) {
	// all pods created by k8s use k8s.io namespace in containerd
	client, err := containerd.New(socket, containerd.WithDefaultNamespace("k8s.io"))
	if err != nil {
		return nil, err
	}
	containerService := client.ContainerService()

	// Create event service client
	eventsService := client.EventService()

	return &containerdHandle{
		client:           client,
		containerService: containerService,
		eventsService:    eventsService,
		skipSameSpec:     skipSameSpec,
	}, nil
}

func (ch *containerdHandle) watchEvents(ctx context.Context, containerUpdates chan<- string) {
	logger := slogctx.FromCtx(ctx)

	filters := []string{
		`topic=="/containers/update"`,
	}
	eventCh, errCh := ch.eventsService.Subscribe(ctx, filters...)
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

func (ch *containerdHandle) updateContainer(ctx context.Context, id string, containerBurst string) (changed bool, err error) {
	logger := slogctx.FromCtx(ctx)

	burstF, _, err := humanize.ParseSI(containerBurst)
	if err != nil {
		return
	}
	burstTime := time.Duration(float64(time.Second) * burstF)

	ctr, err := ch.client.LoadContainer(ctx, id)
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
	if ch.skipSameSpec && spec.Linux.Resources.CPU.Burst != nil && *spec.Linux.Resources.CPU.Burst == burstMks {
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

	_, err = ch.containerService.Update(ctx, containers.Container{
		ID:   task.ID(),
		Spec: updatedSpec,
	}, "spec")
	if err != nil {
		return
	}
	logger.Debug("spec update successful")

	return
}

func setupEventRecorder(_ context.Context, clientset *kubernetes.Clientset, hostname string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})

	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: hostname})
	return eventRecorder
}
