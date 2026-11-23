package api

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kubearbiter/kubearbiter/internal/webui"
)

// registerUIRoutes 托管内嵌的前端构建产物。
// 未构建前端（只有 .gitkeep）时退化为 JSON 404，与纯 API 模式下行为一致。
func registerUIRoutes(r *gin.Engine, d Deps) {
	sub, err := fs.Sub(webui.Dist, "dist")
	if err != nil {
		return
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		r.NoRoute(func(c *gin.Context) {
			writeError(c, errNotFound())
		})
		return
	}

	fileServer := http.FileServer(http.FS(sub))
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		// API 与探针路径不参与 SPA 回退，避免前端路由吞掉接口的 404。
		if strings.HasPrefix(p, "/api/") || p == "/healthz" || strings.HasPrefix(p, "/webhook/") {
			writeError(c, errNotFound())
			return
		}
		if p != "/" {
			if _, err := fs.Stat(sub, strings.TrimPrefix(p, "/")); err != nil {
				// 命中静态文件才直出；其余交给 index.html 由前端路由处理。
				c.Request.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(c.Writer, c.Request)
	})
}
