package k8swatcher

import (
	"context"
	"fmt"
	"log/slog"
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
	containerToPod      map[string]*podCacheEntry
	ownMetrics          *appmetrics.OwnMetrics
	containerMetrics    *appmetrics.ContainerMetrics
}

type podCacheEntry struct {
	pod    *corev1.Pod
	reader containerdhelper.CgroupBurstReader
	labels prometheus.Labels
	logger *slog.Logger
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
		containerToPod:      map[string]*podCacheEntry{},
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
	logger.Info("starting new watch", "from-version", cu.lastResourceVersion)
	watcher, err = cu.clientset.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{
		Watch:                true,
		SendInitialEvents:    ptr.To(true),
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
			podEntry, ok := cu.containerToPod[id]
			if !ok {
				continue
			}
			pod := podEntry.pod

			callCtx := slogctx.With(ctx, "pod", pod.Name, "namespace", pod.Namespace, "update-type", "restore on containerd event")

			// the program should receive events of its own updates
			// this is fine, because on the second iteration
			// the program will see that there are no changes in burst spec and skip the update

			// we call the full updatePod method because in case we need to emit events, we need full pod context
			changed, err := cu.UpdatePod(callCtx, pod)
			if err != nil {
				panic("previously this pod worked fine but we got an error when updating it on event: pod " + pod.Name + ": " + err.Error())
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
			return fmt.Errorf("unexpected error %T", event.Object)
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
	cacheEntry, ok := cu.containerToPod[id]
	if !ok {
		return
	}
	cu.containerMetrics.CgroupBurstNr.Delete(cacheEntry.labels)
	cu.containerMetrics.CgroupBurstSeconds.Delete(cacheEntry.labels)
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

func stripContainerPrefix(fullId string) (id string, err error) {
	if !strings.HasPrefix(fullId, "containerd://") {
		return "", fmt.Errorf("unexpected container ID prefix: %v", fullId)
	}
	id = strings.TrimPrefix(fullId, "containerd://")
	return
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
	logger.Debug("parsed burst annotation", "value", burstConfig)
	if len(burstConfig) == 0 {
		cu.ownMetrics.PodMissingAnnotationsTotal.Inc()
		logger.Warn("burst annotation is empty")
		return false, nil
	}
	logger.Debug("found matching pod")
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
	logger := slogctx.FromCtx(ctx).With("container", containerStatus.Name)
	var err error
	burstSeconds := 0.0
	burstString, ok := burstConfig[containerStatus.Name]
	if ok {
		delete(burstConfig, containerStatus.Name)
		burstSeconds, _, err = humanize.ParseSI(burstString)
		if err != nil {
			logger.Error("could not parse annotation", "value", burstString, "error", err.Error())
			return false
		}
	}

	id, err := stripContainerPrefix(containerStatus.ContainerID)
	if err != nil {
		logger.Error("could not parse container ID", "value", containerStatus.ContainerID, "error", err.Error())
		return false
	}

	cu.containerMetrics.SpecCgroupBurst.WithLabelValues(cu.appConfig.NodeName, pod.Namespace, pod.Name, containerStatus.Name).Set(burstSeconds)

	changed, err = cu.ContainerdHelper.UpdateContainer(ctx, id, burstSeconds)
	if err != nil {
		cu.ownMetrics.ContainerUpdateAttemptsFailTotal.Inc()
		// error here is not propagated intentionally
		logger.Error("failed to set burst", "value", burstString, "error", err.Error())
		cu.er.Event(pod, corev1.EventTypeWarning, eventContainerError, containerStatus.Name+": unable to set cpu burst: "+err.Error())
		return
	}
	if changed {
		cu.ownMetrics.ContainerUpdateAttemptsSuccessTotal.Inc()
		logger.Info("set burst", "value", burstString)
		cu.er.Event(pod, corev1.EventTypeNormal, eventContainerSet, containerStatus.Name+": set cpu burst to "+burstString)
	} else {
		cu.ownMetrics.ContainerUpdateAttemptsSkipTotal.Inc()
	}
	if burstSeconds == 0 {
		cu.deleteFromCache(id)
		return
	}
	logger.Debug("saving container-to-pod mapping", "id", id)
	gatherMetrics, err := cu.ContainerdHelper.GetCgroupBurstReader(ctx, id)
	if err != nil {
		logger.Error("could not get cgroup reader")
		gatherMetrics = nil
	}
	cacheEntry, ok := cu.containerToPod[id]
	if !ok {
		cacheEntry = &podCacheEntry{
			reader: gatherMetrics,
			labels: prometheus.Labels{
				"node":      cu.appConfig.NodeName,
				"namespace": pod.Namespace,
				"pod":       pod.Name,
				"container": containerStatus.Name,
				"name":      id,
			},
			logger: logger,
		}
		cu.containerToPod[id] = cacheEntry
		cu.ownMetrics.ContainerIdCacheSize.Set(float64(len(cu.containerToPod)))
	}
	cacheEntry.pod = pod
	return
}

func (cu *CgroupUpdater) GatherCgroupBurst(ctx context.Context) error {
	for k, v := range cu.containerToPod {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if v.reader == nil {
			continue
		}
		nrBurst, burstSeconds, err := v.reader()
		if err != nil {
			v.logger.Error("could not read cgroup metrics", "error", err.Error())
			cu.containerMetrics.CgroupBurstNr.Delete(v.labels)
			cu.containerMetrics.CgroupBurstSeconds.Delete(v.labels)
			delete(cu.containerToPod, k)
			continue
		}
		cu.containerMetrics.CgroupBurstNr.With(v.labels).Set(nrBurst)
		cu.containerMetrics.CgroupBurstSeconds.With(v.labels).Set(burstSeconds)
	}
	return nil
}
