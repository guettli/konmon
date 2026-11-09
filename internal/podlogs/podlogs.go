// Package podlogs fetches the last log lines of the container of a finding,
// for the logs annotation of its alert: an alert about a crash-looping
// container then says why it crashed, not only how often.
//
// Logs are fetched only for alerts which are sent, and only once per
// container instance: the result is cached by Pod UID, container and
// restart count, so a firing alert which is re-sent on every evaluation
// does not fetch again until the container restarted.
package podlogs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/guettli/konmon/internal/checks"
)

// Defaults of Fetcher.
const (
	DefaultLines      = 40
	DefaultMaxBytes   = 4096
	DefaultTimeout    = 5 * time.Second
	DefaultErrorRetry = 5 * time.Minute
)

// TruncatedMarker is the first line of logs which were cut to MaxBytes.
const TruncatedMarker = "[... truncated by konmon ...]"

// unusedFor: cache entries which were not asked for this long are dropped.
const unusedFor = time.Hour

// CachedPod returns a Pod of the informer cache. synced false: the cache
// does not know (pods are not watched, or not listed yet). synced true and
// obj nil: the Pod does not exist.
type CachedPod func(namespace, name string) (obj *unstructured.Unstructured, synced bool)

// Fetcher fetches and caches container logs.
type Fetcher struct {
	// Client reads Pods (if Cached does not know them) and their logs.
	Client corev1client.PodsGetter
	// Cached looks the Pod up in the informer cache first. Optional.
	Cached CachedPod
	// Lines is how many lines are fetched (tailLines). Default DefaultLines.
	Lines int
	// MaxBytes caps the logs; longer logs are cut at the front. Default
	// DefaultMaxBytes.
	MaxBytes int
	// Timeout of one fetch. Default DefaultTimeout.
	Timeout time.Duration
	// ErrorRetry: a failed fetch is tried again after this long (or when
	// the container restarted). Default DefaultErrorRetry.
	ErrorRetry time.Duration
	Log        *slog.Logger

	mu    sync.Mutex
	cache map[cacheKey]*entry
}

type cacheKey struct {
	uid, container string
	restartCount   int32
}

type entry struct {
	logs    string
	ok      bool
	failed  bool
	fetched time.Time
	used    time.Time
}

// Logs returns the logs annotation of the finding's alert. ok is false if
// there is none: the finding is not about a Pod, the Pod is gone, no
// container could be picked, or the container never ran (an image pull
// error), so it has no logs.
//
// A finding about the whole Pod (no container, e.g. a failed Job Pod from
// the conditions check) gets the logs of the container which failed, see
// pickContainer. An error is not hidden: it is logged and becomes the annotation,
// "konmon: could not fetch logs: <error>".
func (p *Fetcher) Logs(ctx context.Context, f *checks.Finding, now time.Time) (logs string, ok bool) {
	if f.GVR.Group != "" || f.GVR.Resource != "pods" || f.Name == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	pod, err := p.pod(ctx, f.Namespace, f.Name)
	if err != nil {
		p.logError(f, err)
		return "konmon: could not fetch logs: " + err.Error(), true
	}
	if pod == nil || string(pod.UID) != f.UID {
		// Gone (or recreated with the same name): the alert resolves soon.
		return "", false
	}
	container := f.Container
	if container == "" {
		container = pickContainer(pod)
		if container == "" {
			return "", false
		}
	}
	status := containerStatus(pod, container)
	if status == nil {
		return "", false
	}
	previous := status.LastTerminationState.Terminated != nil
	if !previous && status.State.Running == nil && status.State.Terminated == nil {
		// Never started: no logs. The waiting reason is in the finding.
		return "", false
	}
	key := cacheKey{uid: string(pod.UID), container: container, restartCount: status.RestartCount}

	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[cacheKey]*entry{}
	}
	for k, e := range p.cache {
		if now.Sub(e.used) > unusedFor {
			delete(p.cache, k)
		}
	}
	if e, hit := p.cache[key]; hit && (!e.failed || now.Sub(e.fetched) < p.errorRetry()) {
		e.used = now
		p.mu.Unlock()
		return e.logs, e.ok
	}
	p.mu.Unlock()

	e := &entry{ok: true, fetched: now, used: now}
	body, cut, err := p.fetch(ctx, pod.Namespace, pod.Name, container, previous)
	if err != nil {
		p.logErrorContainer(f, container, err)
		e.failed = true
		e.logs = "konmon: could not fetch logs: " + err.Error()
	} else {
		instance := "current instance"
		if previous {
			instance = "previous instance"
		}
		e.logs = fmt.Sprintf("konmon: last %d lines of container %s, %s (restartCount %d)\n%s",
			p.lines(), container, instance, status.RestartCount, Truncate(body, p.maxBytes(), cut))
	}
	p.mu.Lock()
	p.cache[key] = e
	p.mu.Unlock()
	return e.logs, e.ok
}

func (p *Fetcher) logError(f *checks.Finding, err error) {
	p.logErrorContainer(f, f.Container, err)
}

func (p *Fetcher) logErrorContainer(f *checks.Finding, container string, err error) {
	log := p.Log
	if log == nil {
		log = slog.Default()
	}
	log.Error("could not fetch container logs for alert", "namespace", f.Namespace, "pod", f.Name,
		"container", container, "check", f.Check, "err", err)
}

// pod returns the Pod, nil if it does not exist.
func (p *Fetcher) pod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	if p.Cached != nil {
		if obj, synced := p.Cached(namespace, name); synced {
			if obj == nil {
				return nil, nil
			}
			var pod corev1.Pod
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &pod); err != nil {
				return nil, fmt.Errorf("decode pod %s/%s: %w", namespace, name, err)
			}
			return &pod, nil
		}
	}
	pod, err := p.Client.Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", namespace, name, err)
	}
	return pod, nil
}

// fetch returns the logs; cut: only the end was kept.
func (p *Fetcher) fetch(ctx context.Context, namespace, pod, container string, previous bool) (logs string, cut bool, err error) {
	lines := int64(p.lines())
	stream, err := p.Client.Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
		TailLines: &lines,
	}).Stream(ctx)
	if err != nil {
		return "", false, fmt.Errorf("logs of %s/%s container %s (previous=%t): %w", namespace, pod, container, previous, err)
	}
	defer func() { _ = stream.Close() }()
	// tailLines bounds the size, but one line can be huge: keep only the
	// end while reading.
	t := &tail{max: p.maxBytes()}
	if _, err := io.Copy(t, stream); err != nil {
		return "", false, fmt.Errorf("read logs of %s/%s container %s: %w", namespace, pod, container, err)
	}
	return string(t.buf), t.truncated, nil
}

// tail keeps the last bytes written to it: at least max, at most 2*max+1.
type tail struct {
	max       int
	buf       []byte
	truncated bool
}

func (t *tail) Write(b []byte) (int, error) {
	t.buf = append(t.buf, b...)
	if len(t.buf) > 2*t.max+1 {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max-1:]...)
		t.truncated = true
	}
	return len(b), nil
}

// Truncate cuts logs longer than maxBytes at the front, at a line start if
// there is one in the kept part, and puts TruncatedMarker in front. cut:
// the front is already missing.
func Truncate(logs string, maxBytes int, cut bool) string {
	if len(logs) > maxBytes {
		logs = logs[len(logs)-maxBytes:]
		cut = true
	}
	if cut {
		if i := strings.IndexByte(logs, '\n'); i >= 0 && i < len(logs)-1 {
			logs = logs[i+1:]
		} else {
			for logs != "" && !utf8.RuneStart(logs[0]) {
				logs = logs[1:]
			}
		}
		logs = TruncatedMarker + "\n" + logs
	}
	return strings.ToValidUTF8(logs, "�")
}

// pickContainer chooses the container of a finding about the whole Pod: the
// first container (init containers first, they run first) whose current or
// previous instance terminated with a non-zero exit code; if none failed and
// the Pod has a single container, that one. "" if there is no clear choice.
func pickContainer(pod *corev1.Pod) string {
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for i := range list {
			if failed(list[i].State.Terminated) || failed(list[i].LastTerminationState.Terminated) {
				return list[i].Name
			}
		}
	}
	if len(pod.Spec.Containers) == 1 && len(pod.Spec.InitContainers) == 0 {
		return pod.Spec.Containers[0].Name
	}
	return ""
}

func failed(t *corev1.ContainerStateTerminated) bool {
	return t != nil && t.ExitCode != 0
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for _, list := range [][]corev1.ContainerStatus{
		pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses,
	} {
		for i := range list {
			if list[i].Name == name {
				return &list[i]
			}
		}
	}
	return nil
}

func (p *Fetcher) lines() int {
	if p.Lines > 0 {
		return p.Lines
	}
	return DefaultLines
}

func (p *Fetcher) maxBytes() int {
	if p.MaxBytes > 0 {
		return p.MaxBytes
	}
	return DefaultMaxBytes
}

func (p *Fetcher) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return DefaultTimeout
}

func (p *Fetcher) errorRetry() time.Duration {
	if p.ErrorRetry > 0 {
		return p.ErrorRetry
	}
	return DefaultErrorRetry
}
