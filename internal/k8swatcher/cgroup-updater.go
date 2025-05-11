package k8swatcher

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"meoe.io/cgroup-burst/internal/appconfig"
	"meoe.io/cgroup-burst/internal/appmetrics"
	"meoe.io/cgroup-burst/internal/containerdhelper"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
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
	ownMetrics          *appmetrics.OwnMetrics
	containerMetrics    *appmetrics.ContainerMetrics
}

func setupEventRecorder(_ context.Context, clientset *kubernetes.Clientset, hostname string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})

	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: hostname})
	return eventRecorder
}

func CreateCgroupUpdater(ctx context.Context, clientset *kubernetes.Clientset, appConfig appconfig.AppConfig, ownMetrics *appmetrics.OwnMetrics, containerMetrics *appmetrics.ContainerMetrics) (*CgroupUpdater, error) {
	ch, err := containerdhelper.CreateContainerdHandle(appConfig.ContainerdSocket, appConfig.SkipSameSpec, ownMetrics, appConfig.CgroupRoot, appConfig.ProcRoot)
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
		ownMetrics:          ownMetrics,
		containerMetrics:    containerMetrics,
	}, nil
}

func (cu *CgroupUpdater) Close() {
	cu.ContainerdHelper.Close()
}

func (cu *CgroupUpdater) createWatcher(ctx context.Context) (watcher watch.Interface, err error) {
	logger := slogctx.FromCtx(ctx)

	cu.ownMetrics.K8sWatchStreamsTotal.Inc()
	sendInitialEvents := false
	if cu.lastResourceVersion == "0" {
		sendInitialEvents = true
	}
	logger.Info("starting new watch", "from-version", cu.lastResourceVersion)
	watcher, err = cu.clientset.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{
		Watch:                true,
		SendInitialEvents:    &sendInitialEvents,
		AllowWatchBookmarks:  true,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		ResourceVersion:      cu.lastResourceVersion,
		TimeoutSeconds:       ptr.To(int64(cu.appConfig.WatchTimeout.Seconds())),
		FieldSelector:        "spec.nodeName=" + cu.appConfig.NodeName,
		LabelSelector:        cu.appConfig.LabelSelector,
	})
	if err == nil {
		return
	}
	if apierrors.IsGone(err) || apierrors.IsResourceExpired(err) {
		if cu.lastResourceVersion == "0" {
			// prevent infinite loop
			return
		}
		// lastResourceVersion has expired
		cu.lastResourceVersion = "0"
		// purge all caches to avoid missing updates
		cu.containerToPod = map[string]*corev1.Pod{}
		cu.containerMetrics.GetRequestChan() <- appmetrics.MetricOperation{
			Operation: appmetrics.CachePurge,
		}
		return cu.createWatcher(ctx)
	}
	return
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
				break
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
				break
			}

			callCtx := slogctx.With(ctx, "pod", pod.Name, "namespace", pod.Namespace, "update-type", "restore on containerd event")

			// the program could receive events of its own updates
			// this is fine, because on the second iteration
			// the program will see that there are no changes in burst spec and skip the update

			// we call the full updatePod method because in case we need to emit events, we need full pod context
			changed, err := cu.UpdatePod(callCtx, pod)
			if err != nil {
				cu.ownMetrics.PodUpdatesContainerdFailTotal.Inc()
				break
			}
			if changed {
				cu.ownMetrics.PodUpdatesContainerdSuccessTotal.Inc()
			} else {
				cu.ownMetrics.PodUpdatesContainerdSkipTotal.Inc()
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
			return fmt.Errorf("could not parse pod bookmark: %T", event.Object)
		}
		cu.ownMetrics.K8sWatchBookmarksTotal.Inc()
		cu.lastResourceVersion = pod.ResourceVersion
	case watch.Added, watch.Modified:
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("event object is not a pod: %v", event.Object)
		}
		cu.ownMetrics.K8sWatchUpdatesTotal.Inc()
		cu.handlePodUpdate(ctx, pod)
	case watch.Deleted:
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("event object is not a pod: %v", event.Object)
		}
		cu.ownMetrics.K8sWatchDeletesTotal.Inc()
		cu.handlePodDelete(ctx, pod)
	case watch.Error:
		status, ok := event.Object.(*metav1.Status)
		if !ok {
			return fmt.Errorf("unexpected error type %T", event.Object)
		}
		if status.Reason == metav1.StatusReasonTimeout {
			logger.Debug("watch timeout")
		} else {
			cu.ownMetrics.K8sWatchErrorsTotal.Inc()
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
	changed, err := cu.UpdatePod(ctx, pod)
	if err != nil {
		slogctx.FromCtx(ctx).Error(err.Error())
		cu.er.Event(pod, corev1.EventTypeWarning, eventPodError, err.Error())
		cu.ownMetrics.PodUpdatesK8sFailTotal.Inc()
		// error here is not propagated intentionally
		return
	}
	if changed {
		cu.ownMetrics.PodUpdatesK8sSuccessTotal.Inc()
	} else {
		cu.ownMetrics.PodUpdatesK8sSkipTotal.Inc()
	}
}

func (cu *CgroupUpdater) handlePodDelete(ctx context.Context, pod *corev1.Pod) {
	logger := slogctx.FromCtx(ctx).With("namespace", pod.Namespace, "pod", pod.Name)
	for _, containerStatus := range pod.Status.ContainerStatuses {
		_ = cu.containerMetrics.SpecCgroupBurst.DeleteLabelValues(cu.appConfig.NodeName, pod.Namespace, pod.Name, containerStatus.Name)
		id, err := stripContainerPrefix(containerStatus.ContainerID)
		if err != nil {
			// error here is not propagated intentionally
			logger.Error("can't parse container ID on pod deletion", "value", containerStatus.ContainerID)
			continue
		}
		cu.deleteFromCache(id)
	}
}
func (cu *CgroupUpdater) deleteFromCache(id string) {
	cu.containerMetrics.GetRequestChan() <- appmetrics.MetricOperation{
		Operation: appmetrics.CacheRemove,
		Id:        id,
	}
	delete(cu.containerToPod, id)
	cu.ownMetrics.ContainerIdCacheSize.Set(float64(len(cu.containerToPod)))
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

func stripContainerPrefix(fullId string) (string, error) {
	const containerdPrefix = "containerd://"
	if !strings.HasPrefix(fullId, containerdPrefix) {
		return "", fmt.Errorf("unexpected container ID prefix: %v", fullId)
	}
	id := strings.TrimPrefix(fullId, containerdPrefix)
	return id, nil
}

func (cu *CgroupUpdater) UpdatePod(ctx context.Context, pod *corev1.Pod) (changed bool, err error) {
	logger := slogctx.FromCtx(ctx)
	pa := pod.Annotations
	if pa == nil {
		cu.ownMetrics.PodMissingAnnotationsTotal.Inc()
		return false, fmt.Errorf("burst config is missing from annotations")
	}
	burstConfigRaw, ok := pa[cu.appConfig.BurstAnnotation]
	if !ok {
		cu.ownMetrics.PodMissingAnnotationsTotal.Inc()
		return false, fmt.Errorf("burst config is missing from annotations")
	}
	burstConfig, err := parseMultiConfig(burstConfigRaw)
	if err != nil {
		return false, errors.Wrapf(err, "could not parse burst annotation")
	}
	if len(burstConfig) == 0 {
		cu.ownMetrics.PodMissingAnnotationsTotal.Inc()
		logger.Warn("burst annotation is empty")
		return false, nil
	}
	logger.Info("found matching pod", "burst-annotation", burstConfig)
	for _, containerStatus := range pod.Status.ContainerStatuses {
		containerChanged := cu.updateContainer(ctx, pod, containerStatus, burstConfig)
		changed = changed || containerChanged
	}
	if len(burstConfig) != 0 {
		logger.Warn("part of annotation is not used", "remaining", burstConfig)
		cu.ownMetrics.PodUnusedAnnotationsTotal.Inc()
	}

	return
}

func (cu *CgroupUpdater) updateContainer(ctx context.Context, pod *corev1.Pod, containerStatus corev1.ContainerStatus, burstConfig map[string]string) (changed bool) {
	ctx = slogctx.With(ctx, "container", containerStatus.Name)
	logger := slogctx.FromCtx(ctx)

	var err error
	burstSeconds := 0.0
	burstString, ok := burstConfig[containerStatus.Name]
	if ok {
		delete(burstConfig, containerStatus.Name)
		burstSeconds, _, err = humanize.ParseSI(burstString)
		if err != nil {
			logger.Error("could not parse annotation", "burst-string", burstString, "error", err.Error())
			return false
		}
	}

	id, err := stripContainerPrefix(containerStatus.ContainerID)
	if err != nil {
		logger.Error("could not parse container ID", "raw-container-id", containerStatus.ContainerID, "error", err.Error())
		return false
	}

	ctx = slogctx.With(ctx, "container-id", id, "burst-string", burstString)
	logger = slogctx.FromCtx(ctx)

	cu.containerMetrics.SpecCgroupBurst.WithLabelValues(cu.appConfig.NodeName, pod.Namespace, pod.Name, containerStatus.Name).Set(burstSeconds)

	changed, err = cu.ContainerdHelper.UpdateContainer(ctx, id, burstSeconds)
	if err != nil {
		cu.ownMetrics.ContainerUpdateAttemptsFailTotal.Inc()
		// error here is not propagated intentionally
		logger.Error("failed to set burst", "error", err.Error())
		cu.er.Event(pod, corev1.EventTypeWarning, eventContainerError, containerStatus.Name+": unable to set cpu burst: "+err.Error())
		return
	}
	if changed {
		cu.ownMetrics.ContainerUpdateAttemptsSuccessTotal.Inc()
		logger.Info("set burst successfully")
		cu.er.Event(pod, corev1.EventTypeNormal, eventContainerSet, containerStatus.Name+": set cpu burst to "+burstString)
	} else {
		cu.ownMetrics.ContainerUpdateAttemptsSkipTotal.Inc()
	}
	if burstSeconds == 0 {
		cu.deleteFromCache(id)
		return
	}

	cu.containerToPod[id] = pod
	cu.ownMetrics.ContainerIdCacheSize.Set(float64(len(cu.containerToPod)))

	gatherMetrics, err := cu.ContainerdHelper.GetCgroupBurstReader(ctx, id)
	if err != nil {
		logger.Error("could not create cgroup reader")
		cu.containerMetrics.GetRequestChan() <- appmetrics.MetricOperation{
			Operation: appmetrics.CacheRemove,
			Id:        id,
		}
		return
	}
	cu.containerMetrics.GetRequestChan() <- appmetrics.MetricOperation{
		Operation: appmetrics.CacheAdd,
		Id:        id,
		Labels:    prometheus.Labels{
			"node":      cu.appConfig.NodeName,
			"namespace": pod.Namespace,
			"pod":       pod.Name,
			"container": containerStatus.Name,
			"name":      id,
		},
		Update:    gatherMetrics,
	}
	return
}
