package k8swatcher

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"meoe.io/cgroup-burst/internal/appconfig"
	"meoe.io/cgroup-burst/internal/containerdhelper"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/pkg/errors"
	slogctx "github.com/veqryn/slog-context"
)

const (
	eventPodError       = "CgroupPodError"
	eventContainerError = "CgroupContainerError"
	eventContainerSet   = "CgroupBurstSet"
)

type CgroupUpdater struct {
	appConfig           appconfig.AppConfig
	clientset           *kubernetes.Clientset
	ContainerdHelper    *containerdhelper.ContainerdHelper
	er                  record.EventRecorder
	lastResourceVersion string
	containerToPod      map[string]*corev1.Pod
}

func setupEventRecorder(_ context.Context, clientset *kubernetes.Clientset, hostname string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})

	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: hostname})
	return eventRecorder
}

func CreateCgroupUpdater(ctx context.Context, clientset *kubernetes.Clientset, appConfig appconfig.AppConfig) (*CgroupUpdater, error) {
	ch, err := containerdhelper.CreateContainerdHandle(appConfig.ContainerdSocket, appConfig.SkipSameSpec)
	if err != nil {
		return nil, err
	}
	er := setupEventRecorder(ctx, clientset, appConfig.Hostname)
	return &CgroupUpdater{
		appConfig:           appConfig,
		clientset:           clientset,
		ContainerdHelper:    ch,
		er:                  er,
		lastResourceVersion: "0",
		containerToPod:      map[string]*corev1.Pod{},
	}, nil
}

func (lu *CgroupUpdater) createWatcher(ctx context.Context) (watcher watch.Interface, err error) {
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

func (cu *CgroupUpdater) Watch(ctx context.Context, containerUpdates <-chan string) error {
	watcher, err := cu.createWatcher(ctx)
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
				watcher, err = cu.createWatcher(ctx)
				if err != nil {
					return err
				}
				continue
			}
			err = cu.handlePodEvent(ctx, podEvent)
			if err != nil {
				return err
			}
		case id, ok := <-containerUpdates:
			if !ok {
				return fmt.Errorf("containerd updates channel is closed")
			}
			pod, ok := cu.containerToPod[id]
			if !ok {
				continue
			}

			callCtx := slogctx.With(ctx, "pod", pod.Name, "namespace", pod.Namespace, "update-type", "restore on containerd event")

			// the program should receive events of its own updates
			// this is fine, because on the second iteration
			// the program will see that there are no changes in burst spec and skip the update

			// we call the full handlePod method because in case we need to emit events, we need full pod context
			_, err = cu.handlePod(callCtx, pod)
			if err != nil {
				panic("previously this pod worked fine but we got an error when updating it on event: pod " + pod.Name + ": " + err.Error())
			}
		}
	}
}

func (cu *CgroupUpdater) handlePodEvent(ctx context.Context, event watch.Event) error {
	logger := slogctx.FromCtx(ctx)
	switch event.Type {
	case watch.Bookmark:
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("unexpected error %T", event.Object)
		}
		cu.lastResourceVersion = pod.ResourceVersion
	case watch.Added, watch.Modified:
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("event object is not a pod: %v", event.Object)
		}
		cu.handlePodUpdate(ctx, pod)
	case watch.Deleted:
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("event object is not a pod: %v", event.Object)
		}
		cu.handlePodDelete(pod)
	case watch.Error:
		status, ok := event.Object.(*metav1.Status)
		if !ok {
			return fmt.Errorf("unexpected error type %T", event.Object)
		}
		if status.Reason == metav1.StatusReasonTimeout {
			logger.Debug("watch timeout")
		} else {
			logger.Error("received error status", "value", status)
		}
		return nil
	default:
		return fmt.Errorf("unknown event type: %s", event.Type)
	}
	return nil
}

func (cu *CgroupUpdater) handlePodUpdate(ctx context.Context, pod *corev1.Pod) {
	ctx = slogctx.With(ctx, "pod", pod.Name, "namespace", pod.Namespace)
	_, err := cu.handlePod(ctx, pod)
	if err != nil {
		// error here is not propagated intentionally
		slogctx.FromCtx(ctx).Error(err.Error())
		cu.er.Event(pod, corev1.EventTypeWarning, eventPodError, err.Error())
	}
}

func (cu *CgroupUpdater) handlePodDelete(pod *corev1.Pod) {
	for _, containerStatus := range pod.Status.ContainerStatuses {
		id, err := stripContainerPrefix(containerStatus.ContainerID)
		if err != nil {
			continue
		}
		delete(cu.containerToPod, id)
	}
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

func stripContainerPrefix(fullId string) (id string, err error) {
	if !strings.HasPrefix(fullId, "containerd://") {
		return "", fmt.Errorf("unexpected container ID prefix: %v", fullId)
	}
	id = strings.TrimPrefix(fullId, "containerd://")
	return
}

func (cu *CgroupUpdater) handlePod(ctx context.Context, pod *corev1.Pod) (changed bool, err error) {
	logger := slogctx.FromCtx(ctx)
	pa := pod.Annotations
	if pa == nil {
		return false, fmt.Errorf("burst config is missing from annotations")
	}
	burstConfigRaw, ok := pa[cu.appConfig.BurstAnnotation]
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

		id, err := stripContainerPrefix(containerStatus.ContainerID)
		if err != nil {
			return false, err
		}

		containerChanged, err := cu.ContainerdHelper.UpdateContainer(ctx, id, containerBurst)
		if err != nil {
			// error here is not propagated intentionally
			containerLogger.Error("failed to set burst", "value", containerBurst, "error", err.Error())
			cu.er.Event(pod, corev1.EventTypeWarning, eventContainerError, containerStatus.Name+": unable to set cpu burst: "+err.Error())
			continue
		}
		if containerChanged {
			changed = true
			containerLogger.Info("set burst", "value", containerBurst)
			cu.er.Event(pod, corev1.EventTypeNormal, eventContainerSet, containerStatus.Name+": set cpu burst to "+containerBurst)
		}
		containerLogger.Debug("saving container-to-pod mapping", "id", id)
		cu.containerToPod[id] = pod
	}

	return
}
