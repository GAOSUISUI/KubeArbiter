// Package metrics 维护节点实际用量的时间序列聚合，供决策核的 load 维度使用。
// 采集源是 kubelet 的 stats/summary（经 API Server 的 nodes/proxy 转发），
// 这里只做"样本 -> 分钟级 EWMA"，不承担任何调度语义。
//
// 除 EWMA 外，每个节点还保留一段固定长度的原始样本环形缓冲，供监控大盘与
// 健康分查询短期趋势。缓冲只在进程内，重启即丢失；需要长期历史时应接 Prometheus。
package metrics

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

const (
	// TauSeconds 是 EWMA 的时间常数：5 分钟。dt 越大新样本权重越高，
	// 等价于 alpha = 1 - exp(-dt/tau)，15s 采集间隔下 alpha ≈ 0.049。
	TauSeconds = 300.0
	// DefaultRetention 是原始样本的保留时长；15s 采样下约 1440 个点。
	DefaultRetention = 6 * time.Hour
	// historyCapacity 是每节点样本上限，留出余量避免频繁切片扩容。
	historyCapacity = 2048
)

// Sample 是一条原始采集样本。
type Sample struct {
	At    time.Time
	Usage schedcore.Resources
}

type nodeSample struct {
	usage   schedcore.Resources
	at      time.Time
	history []Sample // 按时间升序，末尾最新
}

// Store 保存每个节点的 EWMA 用量与短期原始样本。并发安全：采集协程写，决策/查询路径读。
type Store struct {
	mu        sync.RWMutex
	nodes     map[string]*nodeSample
	newest    time.Time
	retention time.Duration
}

func NewStore() *Store {
	return NewStoreWithRetention(DefaultRetention)
}

// NewStoreWithRetention 允许调用方调整样本保留时长，非正值回退到默认值。
func NewStoreWithRetention(retention time.Duration) *Store {
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &Store{
		nodes:     make(map[string]*nodeSample),
		retention: retention,
	}
}

// Observe 写入一条原始样本并更新该节点的 EWMA。
// 首个样本直接作为初值（避免从 0 缓慢爬升）；乱序或同一时刻的样本被忽略，
// 保证结果只与样本序列有关、可复现。
func (s *Store) Observe(node string, usage schedcore.Resources, at time.Time) {
	if node == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, ok := s.nodes[node]
	if !ok {
		s.nodes[node] = &nodeSample{
			usage:   usage,
			at:      at,
			history: []Sample{{At: at, Usage: usage}},
		}
	} else {
		dt := at.Sub(prev.at).Seconds()
		if dt <= 0 {
			return
		}
		alpha := 1 - math.Exp(-dt/TauSeconds)
		prev.usage = schedcore.Resources{
			CPUMilli: ewma(prev.usage.CPUMilli, usage.CPUMilli, alpha),
			MemBytes: ewma(prev.usage.MemBytes, usage.MemBytes, alpha),
		}
		prev.at = at
		prev.history = append(prev.history, Sample{At: at, Usage: usage})
		if len(prev.history) > historyCapacity {
			prev.history = append([]Sample(nil), prev.history[len(prev.history)-historyCapacity:]...)
		}
		if cutoff := at.Add(-s.retention); len(prev.history) > 1 {
			drop := 0
			for drop < len(prev.history)-1 && prev.history[drop].At.Before(cutoff) {
				drop++
			}
			if drop > 0 {
				prev.history = append([]Sample(nil), prev.history[drop:]...)
			}
		}
	}
	if at.After(s.newest) {
		s.newest = at
	}
}

// NodeUsage 返回节点当前的 EWMA 用量与最后一次样本时间。
func (s *Store) NodeUsage(node string) (schedcore.Resources, time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sample, ok := s.nodes[node]
	if !ok {
		return schedcore.Resources{}, time.Time{}, false
	}
	return sample.usage, sample.at, true
}

// Newest 返回全部节点中最新样本的时间；零值表示还没有任何指标。
func (s *Store) Newest() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.newest
}

// Retention 返回原始样本保留时长，供接口向调用方说明历史范围。
func (s *Store) Retention() time.Duration {
	return s.retention
}

// Nodes 返回已有样本的节点名，按字典序排列。
func (s *Store) Nodes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.nodes))
	for name := range s.nodes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Series 返回指定节点在 [from, to) 内的原始样本副本；from 为零值时返回全部保留样本。
func (s *Store) Series(node string, from, to time.Time) []Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sample, ok := s.nodes[node]
	if !ok {
		return nil
	}
	out := make([]Sample, 0, len(sample.history))
	for _, item := range sample.history {
		if !from.IsZero() && item.At.Before(from) {
			continue
		}
		if !to.IsZero() && !item.At.Before(to) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func ewma(prev, cur int64, alpha float64) int64 {
	return int64(math.Round(float64(prev) + alpha*(float64(cur)-float64(prev))))
}
