// arbiter-api 是 KubeArbiter 的 HTTP 入口。
// M1 阶段只挂载只读视图与健康检查，业务写接口在决策核与任务引擎就绪后逐个接入。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kubearbiter/kubearbiter/internal/api"
	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/internal/decision/metrics"
	"github.com/kubearbiter/kubearbiter/internal/store/postgres"
)

func main() {
	gin.SetMode(gin.ReleaseMode)

	addr := os.Getenv("ARBITER_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store := api.NewMemoryStore()
	deps := api.Deps{Policies: store, Decisions: store, Writer: store, Snapshots: store}

	// 有可用 kubeconfig 时才连集群；连不上不阻塞启动，改为在 /healthz 暴露 k8s 依赖不可用。
	if view, err := startClusterView(ctx); err != nil {
		log.Printf("集群视图未启用: %v", err)
	} else {
		deps.Snapshotter = func(context.Context) (capacity.Snapshot, error) {
			return view.Snapshot(time.Now())
		}
		deps.TenantOfNamespace = view.NamespaceTenant
		deps.Checks = map[string]func(context.Context) error{
			"k8s": func(context.Context) error {
				if !view.HasSynced() {
					return errors.New("informer 尚未完成首次同步")
				}
				return nil
			},
		}
	}

	// ARBITER_PG_DSN 缺省时不连数据库，此时退化为内存存储（进程内可跑模拟，重启即丢）。
	if dsn := os.Getenv("ARBITER_PG_DSN"); dsn != "" {
		pool, err := postgres.New(ctx, dsn)
		if err != nil {
			log.Printf("PostgreSQL 未启用: %v", err)
		} else {
			defer pool.Close()
			pg := api.NewPGStore(pool)
			deps.Policies, deps.PolicyWriter = pg, pg
			deps.Decisions, deps.Writer = pg, pg
			deps.Tenants, deps.Snapshots, deps.TenantCatalog = pg, pg, pg
			if deps.Checks == nil {
				deps.Checks = map[string]func(context.Context) error{}
			}
			deps.Checks["postgres"] = func(ctx context.Context) error {
				pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				return pool.Ping(pingCtx)
			}
		}
	}

	router := api.NewRouter(deps)
	srv := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("arbiter-api listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	// admission webhook 需要 TLS：只有同时提供证书与私钥才启动，端口默认 :9443。
	if cert, key := os.Getenv("ARBITER_TLS_CERT"), os.Getenv("ARBITER_TLS_KEY"); cert != "" && key != "" {
		webhookAddr := os.Getenv("ARBITER_WEBHOOK_ADDR")
		if webhookAddr == "" {
			webhookAddr = ":9443"
		}
		webhookSrv := &http.Server{Addr: webhookAddr, Handler: router, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			log.Printf("arbiter-webhook listening on %s (tls)", webhookAddr)
			if err := webhookSrv.ListenAndServeTLS(cert, key); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("webhook listen: %v", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = webhookSrv.Shutdown(shutdownCtx)
		}()
	}

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Print("arbiter-api stopped")
}

// startClusterView 按 KUBECONFIG / ~/.kube/config 建立 clientset，并异步启动 informer。
// 返回的 View 在 ctx 取消后停止。
func startClusterView(ctx context.Context) (*capacity.View, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	view := capacity.NewView(client, 5*time.Minute)
	// 指标采集独立于 informer：即使 kubelet 暂时不可达，决策仍会带着
	// METRICS_MISSING 警告降级运行，而不是阻塞启动。
	usage := metrics.NewStore()
	view.UseMetrics(usage)
	go metrics.NewCollector(client, usage).Run(ctx)
	go func() {
		if err := view.Run(ctx); err != nil {
			log.Printf("informer 停止: %v", err)
		}
	}()
	return view, nil
}
