package metrics

import (
	"math"
	"sync"
	"time"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

const (
	// PodUsageGCAfter 是 per-pod 用量的保留窗口：Pod 消失 15 分钟后清理条目。
	PodUsageGCAfter = 15 * time.Minute
)

// PodKey 定位一个 Pod 的用量条目。
type PodKey struct {
	Namespace string
	Name      string
}

type podUsageEntry struct {
	usage    schedcore.Resources
	at       time.Time // 最近一次 EWMA 更新时间
	lastSeen time.Time // 最近一次在 summary 中出现的时间，用于 GC
}

// PodUsageStore 维护 per-pod 实际用量的 EWMA。与节点级 Store 分开：
// 这里只保留"当前值"，不保留历史序列，供 rightsizing 与监控使用。
type PodUsageStore struct {
	mu   sync.RWMutex
	pods map[PodKey]*podUsageEntry
}

func NewPodUsageStore() *PodUsageStore {
	return &PodUsageStore{pods: make(map[PodKey]*podUsageEntry)}
}

// ObservePods 写入一个节点上所有 Pod 的本轮用量。
// first 观察直接取初值；后续按 EWMA 平滑（复用节点级 TauSeconds）。
// 不在本轮出现的 Pod 保留 lastSeen，由 GC 按窗口清理。
func (s *PodUsageStore) ObservePods(node string, usages map[PodKey]schedcore.Resources, at time.Time) {
	if node == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, usage := range usages {
		entry, ok := s.pods[key]
		if !ok {
			s.pods[key] = &podUsageEntry{usage: usage, at: at, lastSeen: at}
			continue
		}
		if dt := at.Sub(entry.at).Seconds(); dt > 0 {
			alpha := 1 - math.Exp(-dt/TauSeconds)
			entry.usage = schedcore.Resources{
				CPUMilli: ewma(entry.usage.CPUMilli, usage.CPUMilli, alpha),
				MemBytes: ewma(entry.usage.MemBytes, usage.MemBytes, alpha),
			}
			entry.at = at
		}
		entry.lastSeen = at
	}

	// 惰性 GC：写路径顺带清理消失超过窗口的 Pod。
	if len(s.pods) > 0 {
		cutoff := at.Add(-PodUsageGCAfter)
		for key, entry := range s.pods {
			if entry.lastSeen.Before(cutoff) {
				delete(s.pods, key)
			}
		}
	}
}

// PodUsage 返回一个 Pod 当前的 EWMA 用量与最后更新时间。
func (s *PodUsageStore) PodUsage(key PodKey) (schedcore.Resources, time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.pods[key]
	if !ok {
		return schedcore.Resources{}, time.Time{}, false
	}
	return entry.usage, entry.at, true
}

// Snapshot 返回全部 Pod 用量的副本，供 rightsizing 批量读取。
func (s *PodUsageStore) Snapshot() map[PodKey]schedcore.Resources {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[PodKey]schedcore.Resources, len(s.pods))
	for key, entry := range s.pods {
		out[key] = entry.usage
	}
	return out
}
