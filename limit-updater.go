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
	"github.com/containerd/containerd/containers"
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
}

func createLimitUpdater(ctx context.Context, clientset *kubernetes.Clientset, appConfig AppConfig) (*limitUpdater, error) {
	ch, err := createContainerdHandle(appConfig.ContainerdSocket)
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
	}, nil
}

func (lu *limitUpdater) runAndWatch(ctx context.Context) error {
	logger := slogctx.FromCtx(ctx)
	logger.Info("starting new watch", "from version", lu.lastResourceVersion)
	podsWatcher, err := lu.clientset.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{
		SendInitialEvents:    ptr.To(true),
		AllowWatchBookmarks:  true,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		ResourceVersion:      lu.lastResourceVersion,
		TimeoutSeconds:       ptr.To(int64(lu.appConfig.WatchTimeout.Seconds())),
		FieldSelector:        "spec.nodeName=" + lu.appConfig.NodeName,
		LabelSelector:        lu.appConfig.LabelSelector,
	})
	if err != nil {
		return nil
	}
	defer podsWatcher.Stop()

	return lu.watch(ctx, podsWatcher)
}

func (lu *limitUpdater) watch(ctx context.Context, watcher watch.Interface) error {
	logger := slogctx.FromCtx(ctx)
	for event := range watcher.ResultChan() {
		switch event.Type {
		case watch.Bookmark:
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			logger.Debug("bookmark received", "resource version", pod.ResourceVersion)
			lu.lastResourceVersion = pod.ResourceVersion
		case watch.Added:
			err := lu.handlePodEvent(ctx, event)
			if err != nil {
				return err
			}
		case watch.Modified:
			err := lu.handlePodEvent(ctx, event)
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
	}
	return fmt.Errorf("watch channel ended unexpectedly")
}

func (lu *limitUpdater) handlePodEvent(ctx context.Context, event watch.Event) error {
	pod, ok := event.Object.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("event object is not a pod: %T", event.Object)
	}
	logger := slogctx.FromCtx(ctx)
	podLogger := logger.With("pod", pod.Name, "namespace", pod.Namespace)
	podCtx := slogctx.NewCtx(ctx, podLogger)
	_, err := lu.handlePod(podCtx, pod)
	if err != nil {
		podLogger.Error(err.Error())
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
	if len(burstConfig) == 0 {
		logger.Warn("burst annotation is empty")
		return false, nil
	}
	logger.Debug("found matching pod")
	for _, containerStatus := range pod.Status.ContainerStatuses {
		containerBurst, ok := burstConfig[containerStatus.Name]
		if !ok {
			continue
		}
		containerLogger := logger.With("container", containerStatus.Name)

		containerChanged, err := lu.handleContainer(ctx, containerBurst, containerStatus.ContainerID)
		changed = changed || containerChanged
		if err == nil {
			if containerChanged {
				containerLogger.Info("set burst", "value", containerBurst)
				lu.er.Event(pod, corev1.EventTypeNormal, eventContainerSet, containerStatus.Name+": set cpu burst to "+containerBurst)
			}
		} else {
			containerLogger.Error("failed to set burst", "value", containerBurst, "error", err.Error())
			lu.er.Event(pod, corev1.EventTypeWarning, eventContainerError, containerStatus.Name+": unable to set cpu burst: "+err.Error())
		}
	}

	return
}

func (lu *limitUpdater) handleContainer(ctx context.Context, containerBurst string, fullId string) (changed bool, err error) {
	burstF, _, err := humanize.ParseSI(containerBurst)
	if err != nil {
		return
	}
	burstTime := time.Duration(float64(time.Second) * burstF)

	if !strings.HasPrefix(fullId, "containerd://") {
		return false, fmt.Errorf("unexpected container ID prefix: %v", fullId)
	}
	id := strings.TrimPrefix(fullId, "containerd://")
	changed, err = lu.ch.updateContainer(ctx, id, burstTime)
	if err != nil {
		return false, errors.Wrap(err, "updateTask")
	}
	return
}

type containerdHandle struct {
	client           *containerd.Client
	containerService containers.Store
}

func createContainerdHandle(socket string) (*containerdHandle, error) {
	client, err := containerd.New(socket, containerd.WithDefaultNamespace("k8s.io"))
	if err != nil {
		return nil, err
	}
	containerService := client.ContainerService()

	return &containerdHandle{
		client:           client,
		containerService: containerService,
	}, nil
}

func (ch *containerdHandle) updateContainer(ctx context.Context, containerId string, burstTime time.Duration) (changed bool, err error) {
	logger := slogctx.FromCtx(ctx)
	ctr, err := ch.client.LoadContainer(ctx, containerId)
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
	if spec.Linux.Resources.CPU.Burst != nil && *spec.Linux.Resources.CPU.Burst == burstMks {
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

func setupEventRecorder(ctx context.Context, clientset *kubernetes.Clientset, hostname string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})

	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: hostname})
	return eventRecorder
}
