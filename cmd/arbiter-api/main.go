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
	deps := api.Deps{Policies: store, Decisions: store}

	// 有可用 kubeconfig 时才连集群；连不上不阻塞启动，改为在 /healthz 暴露 k8s 依赖不可用。
	if view, err := startClusterView(ctx); err != nil {
		log.Printf("集群视图未启用: %v", err)
	} else {
		deps.Checks = map[string]func(context.Context) error{
			"k8s": func(context.Context) error {
				if !view.HasSynced() {
					return errors.New("informer 尚未完成首次同步")
				}
				return nil
			},
		}
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("arbiter-api listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

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
	go func() {
		if err := view.Run(ctx); err != nil {
			log.Printf("informer 停止: %v", err)
		}
	}()
	return view, nil
}
