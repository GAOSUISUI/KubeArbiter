// arbiter-worker 消费 tasks 表：领取已审批的任务，先 dry-run 再写集群，
// 最后把状态、日志与审计写回。没有可用 kubeconfig 时进程仍可启动，但任务会
// 因"集群不可用"重试直至失败，绝不返回假成功。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kubearbiter/kubearbiter/internal/actuator"
	"github.com/kubearbiter/kubearbiter/internal/observe"
	"github.com/kubearbiter/kubearbiter/internal/store/postgres"
	"github.com/kubearbiter/kubearbiter/internal/taskengine"
	"github.com/kubearbiter/kubearbiter/internal/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("ARBITER_PG_DSN")
	if dsn == "" {
		log.Fatal("ARBITER_PG_DSN 未配置：worker 必须与 API 共享任务表")
	}
	pool, err := postgres.New(ctx, dsn)
	if err != nil {
		log.Fatalf("连接 PostgreSQL: %v", err)
	}
	defer pool.Close()

	cfg := worker.Config{
		ID:              workerID(),
		Concurrency:     envInt("ARBITER_WORKER_CONCURRENCY", 2),
		PollInterval:    envDuration("ARBITER_WORKER_POLL_INTERVAL", 2*time.Second),
		ReapInterval:    envDuration("ARBITER_WORKER_REAP_INTERVAL", 30*time.Second),
		LeaseGrace:      envDuration("ARBITER_WORKER_LEASE_GRACE", 30*time.Second),
		Retry:           taskengine.DefaultRetryPolicy,
		FinalizeTimeout: 5 * time.Second,
	}
	registry := observe.NewRegistry()
	cfg.Metrics = observe.NewMetrics(registry)

	metricsAddr := os.Getenv("ARBITER_WORKER_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9101"
	}
	metricsSrv := &http.Server{
		Addr:              metricsAddr,
		Handler:           observe.Handler(registry),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("arbiter-worker metrics listening on %s", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics listen: %v", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	w := worker.New(cfg, worker.NewPGStore(pool), buildExecutor())
	log.Printf("arbiter-worker %s 启动：并发 %d，轮询 %s", cfg.ID, cfg.Concurrency, cfg.PollInterval)
	w.Run(ctx)
	log.Print("arbiter-worker stopped")
}

// buildExecutor 优先使用 KUBECONFIG / ~/.kube/config；Pod 内没有 kubeconfig 时
// 回退到 in-cluster 配置。都失败时不阻塞启动，交给 UnavailableExecutor 把
// "不可用"如实写进任务错误。
func buildExecutor() actuator.Executor {
	cfg, err := restConfig()
	if err != nil {
		log.Printf("集群客户端未启用: %v", err)
		return actuator.UnavailableExecutor{Reason: "集群客户端未配置，无法执行集群写操作"}
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Printf("集群客户端初始化失败: %v", err)
		return actuator.UnavailableExecutor{Reason: "集群客户端初始化失败"}
	}
	return actuator.NewKubeExecutor(client)
}

func restConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err == nil {
		return cfg, nil
	}
	inCluster, inErr := rest.InClusterConfig()
	if inErr != nil {
		return nil, fmt.Errorf("kubeconfig: %v; in-cluster: %w", err, inErr)
	}
	return inCluster, nil
}

func workerID() string {
	id := os.Getenv("ARBITER_WORKER_ID")
	if id == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "unknown"
		}
		id = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	// audit_logs.actor 是 varchar(63)，超长会整条写入失败。
	if len(id) > 63 {
		id = id[:63]
	}
	return id
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		log.Printf("%s=%q 非法，使用默认值 %d", key, raw, fallback)
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("%s=%q 非法，使用默认值 %s", key, raw, fallback)
		return fallback
	}
	return d
}
