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

	"github.com/kubearbiter/kubearbiter/internal/aiops"
	"github.com/kubearbiter/kubearbiter/internal/api"
	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/internal/decision/metrics"
	"github.com/kubearbiter/kubearbiter/internal/forecast"
	"github.com/kubearbiter/kubearbiter/internal/observe"
	"github.com/kubearbiter/kubearbiter/internal/slo"
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
	deps := api.Deps{
		Policies: store, Decisions: store, Writer: store, Snapshots: store,
		SLO: store, SLOWriter: store, Idempotency: store,
		Tasks: store, TaskWriter: store, AuditLogs: store, AuditWriter: store,
		Incidents: store, IncidentWriter: store,
	}
	var incidentStore aiops.Store = store
	recorder := observe.NewRecorder()
	deps.Recorder = recorder

	// 有可用 kubeconfig 时才连集群；连不上不阻塞启动，改为在 /healthz 暴露 k8s 依赖不可用。
	if view, usage, podUsage, err := startClusterView(ctx); err != nil {
		log.Printf("集群视图未启用: %v", err)
	} else {
		deps.Snapshotter = func(context.Context) (capacity.Snapshot, error) {
			return view.Snapshot(time.Now())
		}
		deps.Observer = func(context.Context) (capacity.Diagnostics, error) {
			return view.Diagnostics(time.Now())
		}
		deps.Metrics = usage
		deps.PodUsage = podUsage
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
			deps.SLO = pg
			deps.SLOWriter = pg
			deps.Idempotency = pg
			deps.Tasks, deps.TaskWriter = pg, pg
			deps.AuditLogs, deps.AuditWriter = pg, pg
			deps.Incidents, deps.IncidentWriter = pg, pg
			incidentStore = pg
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

	// 平台自身的可用性 SLO：默认自动建定义并采集 /api/v1/ 请求；
	// ARBITER_SELF_SLO=off 可关闭。
	if os.Getenv("ARBITER_SELF_SLO") != "off" {
		ensureSelfSLO(ctx, deps)
		if sink, ok := deps.SLO.(observe.BucketSink); ok {
			go recorder.Run(ctx, sink, "arbiter-api")
		}
	}

	// 故障检测只读集群，写入事件与诊断；没有集群视图时自然不启动。
	if deps.Observer != nil && incidentStore != nil {
		interval := aiops.DefaultInterval
		if raw := os.Getenv("ARBITER_DETECT_INTERVAL"); raw != "" {
			if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
				interval = parsed
			}
		}
		source := aiops.SourceFuncs{
			DiagnosticsFn: deps.Observer,
			TenantsFn: func(ctx context.Context, diag capacity.Diagnostics) ([]aiops.TenantInput, error) {
				return api.TenantInputs(ctx, deps.TenantCatalog, diag), nil
			},
			ForecastsFn: func() []aiops.NodeForecastSummary {
				return nodeForecasts(deps)
			},
		}
		go aiops.NewDetector(incidentStore, source, interval, log.Printf).Run(ctx)
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

// nodeForecasts 为检测规则计算各节点的负载外推摘要。
// 任一数据源缺失时返回 nil，预测规则自然跳过；调用方每轮检测调用一次。
func nodeForecasts(deps api.Deps) []aiops.NodeForecastSummary {
	if deps.Metrics == nil || deps.Observer == nil {
		return nil
	}
	diag, err := deps.Observer(context.Background())
	if err != nil {
		return nil
	}
	now := time.Now()
	from := now.Add(-deps.Metrics.Retention())
	out := make([]aiops.NodeForecastSummary, 0, len(diag.Nodes))
	for _, nd := range diag.Nodes {
		series := deps.Metrics.Series(nd.Name, from, now)
		if len(series) == 0 {
			continue
		}
		nf := forecast.NodeForecastFor(nd.Name, series, nd.Allocatable, now)
		// 取两维中较差的 R²：耗尽时间由先到限的维度决定，保守评估。
		r2 := nf.CPU.R2
		if nf.Mem.R2 < r2 {
			r2 = nf.Mem.R2
		}
		out = append(out, aiops.NodeForecastSummary{
			Name:         nf.Name,
			TimeToFull90: nf.TimeToFull90,
			R2:           r2,
			UsageRatio:   nf.UsageRatio,
		})
	}
	return out
}

// startClusterView 按 KUBECONFIG / ~/.kube/config 建立 clientset，并异步启动 informer。
// 返回的 View 在 ctx 取消后停止。
func startClusterView(ctx context.Context) (*capacity.View, *metrics.Store, *metrics.PodUsageStore, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, nil, err
	}

	view := capacity.NewView(client, 5*time.Minute)
	// 指标采集独立于 informer：即使 kubelet 暂时不可达，决策仍会带着
	// METRICS_MISSING 警告降级运行，而不是阻塞启动。
	usage := metrics.NewStore()
	podUsage := metrics.NewPodUsageStore()
	view.UseMetrics(usage)
	go metrics.NewCollector(client, usage, podUsage).Run(ctx)
	go func() {
		if err := view.Run(ctx); err != nil {
			log.Printf("informer 停止: %v", err)
		}
	}()
	return view, usage, podUsage, nil
}

// ensureSelfSLO 保证 arbiter-api 自身有一个可用的可用性 SLO；不覆盖已有定义。
func ensureSelfSLO(ctx context.Context, deps api.Deps) {
	if deps.SLO == nil || deps.SLOWriter == nil {
		return
	}
	definitions, err := deps.SLO.ListSLODefinitions(ctx)
	if err != nil {
		log.Printf("读取 SLO 定义失败，跳过平台自身 SLO: %v", err)
		return
	}
	for _, def := range definitions {
		if def.Service == "arbiter-api" && def.SLIType == slo.SLIAvailability {
			return
		}
	}
	if _, err := deps.SLOWriter.CreateSLODefinition(ctx, slo.Definition{
		Service: "arbiter-api",
		SLIType: slo.SLIAvailability,
		Target:  0.995,
		Window:  time.Hour,
	}); err != nil {
		log.Printf("创建平台自身 SLO 失败: %v", err)
	}
}
