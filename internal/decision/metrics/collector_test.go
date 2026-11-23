package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// newFakeAPIServer 模拟 API Server：nodes 列表 + nodes/proxy/stats/summary。
// 用真实 client-go 走一遍 URL 拼接与 JSON 解析，避免只测私有函数。
func newFakeAPIServer(t *testing.T, summaries map[string]string) *Collector {
	t.Helper()
	items := make([]corev1.Node, 0, len(summaries))
	for name := range summaries {
		items = append(items, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(corev1.NodeList{Items: items})
	})
	for name, body := range summaries {
		name := name
		body := body
		mux.HandleFunc("/api/v1/nodes/"+name+"/proxy/stats/summary", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				t.Errorf("不应带查询参数: %s", r.URL.RawQuery)
			}
			if !strings.Contains(body, `"node"`) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"kubelet down"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("构造 clientset: %v", err)
	}
	store := NewStore()
	return NewCollector(client, store)
}

func TestCollectorCollectsAllNodes(t *testing.T) {
	c := newFakeAPIServer(t, map[string]string{
		"ka-w1": `{"node":{"cpu":{"usageNanoCores":500000000},"memory":{"workingSetBytes":1073741824}}}`,
		"ka-w2": `{"node":{"cpu":{"usageNanoCores":1500000000},"memory":{"workingSetBytes":2147483648}}}`,
	})
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("采集失败: %v", err)
	}

	for name, want := range map[string]schedcore.Resources{
		"ka-w1": {CPUMilli: 500, MemBytes: 1 << 30},
		"ka-w2": {CPUMilli: 1500, MemBytes: 2 << 30},
	} {
		got, at, ok := c.store.NodeUsage(name)
		if !ok {
			t.Fatalf("%s 应有样本", name)
		}
		if got != want {
			t.Fatalf("%s 用量 = %+v, want %+v", name, got, want)
		}
		if !at.Equal(now) {
			t.Fatalf("%s 样本时间 = %v, want %v", name, at, now)
		}
	}
	if !c.store.Newest().Equal(now) {
		t.Fatalf("Newest = %v, want %v", c.store.Newest(), now)
	}
}

func TestCollectorPartialFailureKeepsOtherNodes(t *testing.T) {
	// ka-bad 的响应体不含 node 字段，服务端返回 500，模拟 kubelet 异常。
	c := newFakeAPIServer(t, map[string]string{
		"ka-w1":  `{"node":{"cpu":{"usageNanoCores":250000000},"memory":{"workingSetBytes":536870912}}}`,
		"ka-bad": `{"error":"kubelet down"}`,
	})

	err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("单个节点失败时应返回错误，便于暴露 RBAC/kubelet 问题")
	}
	if !strings.Contains(err.Error(), "ka-bad") {
		t.Fatalf("错误应包含失败节点名，实际 %v", err)
	}
	got, _, ok := c.store.NodeUsage("ka-w1")
	if !ok || got.CPUMilli != 250 {
		t.Fatalf("正常节点应仍被采集，实际 %+v (ok=%v)", got, ok)
	}
	if _, _, ok := c.store.NodeUsage("ka-bad"); ok {
		t.Fatal("失败节点不应写入样本")
	}
}

func TestCollectorRejectsIncompleteSummary(t *testing.T) {
	c := newFakeAPIServer(t, map[string]string{
		"ka-w1": `{"node":{"cpu":{"usageNanoCores":100000000}}}`,
	})

	if err := c.Collect(context.Background()); err == nil {
		t.Fatal("缺少 memory.workingSetBytes 时应报错")
	}
	if _, _, ok := c.store.NodeUsage("ka-w1"); ok {
		t.Fatal("不完整样本不应写入")
	}
}

func TestCollectorRunStopsOnCancel(t *testing.T) {
	c := newFakeAPIServer(t, map[string]string{
		"ka-w1": `{"node":{"cpu":{"usageNanoCores":100000000},"memory":{"workingSetBytes":104857600}}}`,
	})
	c.interval = 10 * time.Millisecond
	c.logf = func(string, ...any) {}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if _, _, ok := c.store.NodeUsage("ka-w1"); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run 未在启动时立即采集")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 应及时返回")
	}
}
