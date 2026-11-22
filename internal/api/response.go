package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 统一信封，逐字对应 openapi.yaml 的 Envelope / Error。
type envelope struct {
	Code    int    `json:"code"`
	Data    any    `json:"data"`
	TraceID string `json:"trace_id"`
}

type errorBody struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Details []ErrorDetail `json:"details,omitempty"`
	TraceID string        `json:"trace_id"`
}

const traceIDContextKey = "trace_id"

// traceID 返回本次请求的 trace_id；中间件未挂载时为空串而不是伪造一个。
func traceID(c *gin.Context) string {
	if v, ok := c.Get(traceIDContextKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func writeOK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, envelope{Code: CodeOK, Data: data, TraceID: traceID(c)})
}

func writeError(c *gin.Context, err *apiError) {
	c.AbortWithStatusJSON(httpStatus(err.code), errorBody{
		Code:    err.code,
		Message: err.message,
		Details: err.details,
		TraceID: traceID(c),
	})
}
