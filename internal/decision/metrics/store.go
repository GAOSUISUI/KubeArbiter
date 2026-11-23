// Package metrics 维护节点实际用量的时间序列聚合，供决策核的 load 维度使用。
// 采集源是 kubelet 的 stats/summary（经 API Server 的 nodes/proxy 转发），
// 这里只做"样本 -> 分钟级 EWMA"，不承担任何调度语义。
package metrics

import (
	"math"
	"sync"
	"time"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// TauSeconds 是 EWMA 的时间常数：5 分钟。dt 越大新样本权重越高，
// 等价于 alpha = 1 - exp(-dt/tau)，15s 采集间隔下 alpha ≈ 0.049。
const TauSeconds = 300.0

type nodeSample struct {
	usage schedcore.Resources
	at    time.Time
}

// Store 保存每个节点的 EWMA 用量。并发安全：采集协程写，决策路径读。
type Store struct {
	mu     sync.RWMutex
	nodes  map[string]*nodeSample
	newest time.Time
}

func NewStore() *Store {
	return &Store{nodes: make(map[string]*nodeSample)}
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
		s.nodes[node] = &nodeSample{usage: usage, at: at}
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

func ewma(prev, cur int64, alpha float64) int64 {
	return int64(math.Round(float64(prev) + alpha*(float64(cur)-float64(prev))))
}
