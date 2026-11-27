package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

const (
	// DefaultInterval 是采集周期：15s，远小于 60s 的指标过期阈值。
	DefaultInterval = 15 * time.Second
	// DefaultTimeout 是单节点请求的超时。kubelet 代理偶发卡住时不能拖垮整轮采集。
	DefaultTimeout = 3 * time.Second
)

// Collector 周期性从各节点 kubelet 的 stats/summary 采集实际用量。
// 它只依赖标准 clientset：List 节点用 CoreV1，读 summary 走 RESTClient 的 nodes/proxy。
type Collector struct {
	client   kubernetes.Interface
	store    *Store
	podUsage *PodUsageStore
	interval time.Duration
	timeout  time.Duration
	now      func() time.Time
	logf     func(format string, args ...any)
}

func NewCollector(client kubernetes.Interface, store *Store, podUsage *PodUsageStore) *Collector {
	return &Collector{
		client:   client,
		store:    store,
		podUsage: podUsage,
		interval: DefaultInterval,
		timeout:  DefaultTimeout,
		now:      time.Now,
		logf:     log.Printf,
	}
}

// Run 先立即采集一次，之后按周期采集，直到 ctx 取消。
func (c *Collector) Run(ctx context.Context) {
	c.collectOnce(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collectOnce(ctx)
		}
	}
}

func (c *Collector) collectOnce(ctx context.Context) {
	if err := c.Collect(ctx); err != nil {
		c.logf("指标采集失败: %v", err)
	}
}

// Collect 对全部节点做一轮采集。单节点失败不影响其它节点，
// 但只要有失败就返回聚合错误，便于在日志里暴露 RBAC 或 kubelet 异常。
func (c *Collector) Collect(ctx context.Context) error {
	nodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("列出节点: %w", err)
	}

	var errs []error
	for _, node := range nodes.Items {
		nodeCtx, cancel := context.WithTimeout(ctx, c.timeout)
		usage, pods, err := c.fetchSummary(nodeCtx, node.Name)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("节点 %s: %w", node.Name, err))
			continue
		}
		c.store.Observe(node.Name, usage, c.now())
		if c.podUsage != nil {
			c.podUsage.ObservePods(node.Name, pods, c.now())
		}
	}
	return errors.Join(errs...)
}

// fetchSummary 读取 /api/v1/nodes/<name>/proxy/stats/summary，
// 返回节点级用量与按 Pod 聚合的容器用量。
func (c *Collector) fetchSummary(ctx context.Context, node string) (schedcore.Resources, map[PodKey]schedcore.Resources, error) {
	raw, err := c.client.CoreV1().RESTClient().
		Get().
		Resource("nodes").
		Name(node).
		SubResource("proxy").
		Suffix("stats/summary").
		DoRaw(ctx)
	if err != nil {
		return schedcore.Resources{}, nil, fmt.Errorf("读取 stats/summary: %w", err)
	}

	var summary nodeSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return schedcore.Resources{}, nil, fmt.Errorf("解析 stats/summary: %w", err)
	}
	if summary.Node.CPU.UsageNanoCores == nil || summary.Node.Memory.WorkingSetBytes == nil {
		return schedcore.Resources{}, nil, errors.New("stats/summary 缺少 cpu.usageNanoCores 或 memory.workingSetBytes")
	}

	// per-pod 用量：逐容器累加（summary 的 pods[].cpu/memory 本身已是该 Pod 汇总，
	// 但按容器累加能兼容个别 kubelet 把并发容器拆开上报的形态）。
	pods := make(map[PodKey]schedcore.Resources, len(summary.Pods))
	for _, p := range summary.Pods {
		if p.PodRef.Namespace == "" || p.PodRef.Name == "" {
			continue
		}
		var cpuMilli, memBytes int64
		for _, cs := range p.Containers {
			if cs.CPU.UsageNanoCores != nil {
				cpuMilli += *cs.CPU.UsageNanoCores / 1_000_000
			}
			if cs.Memory.WorkingSetBytes != nil {
				memBytes += *cs.Memory.WorkingSetBytes
			}
		}
		key := PodKey{Namespace: p.PodRef.Namespace, Name: p.PodRef.Name}
		pods[key] = schedcore.Resources{CPUMilli: cpuMilli, MemBytes: memBytes}
	}

	return schedcore.Resources{
		// 纳核 -> 毫核，整数除法即可，load 维度只关心百分位量级。
		CPUMilli: *summary.Node.CPU.UsageNanoCores / 1_000_000,
		MemBytes: *summary.Node.Memory.WorkingSetBytes,
	}, pods, nil
}

// nodeSummary 只解析需要的字段。字段用指针以区分"缺失"与"值为 0"。
type nodeSummary struct {
	Node struct {
		CPU struct {
			UsageNanoCores *int64 `json:"usageNanoCores"`
		} `json:"cpu"`
		Memory struct {
			WorkingSetBytes *int64 `json:"workingSetBytes"`
		} `json:"memory"`
	} `json:"node"`
	Pods []podSummary `json:"pods"`
}

// podSummary 是 stats/summary 中单个 Pod 的用量块。
type podSummary struct {
	PodRef struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"podRef"`
	Containers []struct {
		CPU struct {
			UsageNanoCores *int64 `json:"usageNanoCores"`
		} `json:"cpu"`
		Memory struct {
			WorkingSetBytes *int64 `json:"workingSetBytes"`
		} `json:"memory"`
	} `json:"containers"`
}
