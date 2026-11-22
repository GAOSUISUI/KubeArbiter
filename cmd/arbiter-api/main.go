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

	"github.com/kubearbiter/kubearbiter/internal/api"
)

func main() {
	gin.SetMode(gin.ReleaseMode)

	addr := os.Getenv("ARBITER_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	store := api.NewMemoryStore()
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewRouter(api.Deps{Policies: store, Decisions: store}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
