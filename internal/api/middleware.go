package api

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const traceIDHeader = "X-Trace-Id"

// TraceID 为每个请求确定 trace_id：优先复用上游传入的合法值，否则生成 UUIDv4。
// 同时写回响应头，便于网关或前端把日志串起来。
func TraceID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.GetHeader(traceIDHeader))
		if !validTraceID(id) {
			id = uuid.NewString()
		}
		c.Set(traceIDContextKey, id)
		c.Header(traceIDHeader, id)
		c.Next()
	}
}

// validTraceID 限制为 1..64 个 URL 安全字符，避免把任意用户输入写进日志与响应头。
func validTraceID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// Recover 把 panic 收敛为统一信封的 50001，进程不退出。
func Recover() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, _ any) {
		writeError(c, errInternal())
	})
}
