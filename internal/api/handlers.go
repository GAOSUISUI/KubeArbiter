package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	defaultLimit = 20
	maxLimit     = 200
)

// Deps 是 API 层的只读依赖集合，由 cmd/arbiter-api 注入。
type Deps struct {
	Policies  PolicyReader
	Decisions DecisionReader
	// Checks 为健康检查集合，key 为依赖名（如 "postgres"、"k8s"）。
	Checks map[string]func(context.Context) error
}

// NewRouter 注册 M1 阶段的只读路由。/healthz 之外的路由均为契约中已定义路径的子集，
// 未实现的写接口不注册，避免出现"看起来能用但返回假数据"的端点。
func NewRouter(d Deps) *gin.Engine {
	if d.Policies == nil && d.Decisions == nil {
		store := NewMemoryStore()
		d.Policies, d.Decisions = store, store
	}
	if d.Policies == nil {
		d.Policies = NewMemoryStore()
	}
	if d.Decisions == nil {
		d.Decisions = NewMemoryStore()
	}

	r := gin.New()
	r.Use(TraceID(), Recover())

	r.GET("/healthz", healthz(d))

	v1 := r.Group("/api/v1")
	v1.GET("/policies", listPolicies(d))
	v1.GET("/placement/decisions", listDecisions(d))
	v1.GET("/placement/decisions/:id", getDecision(d))

	r.NoRoute(func(c *gin.Context) {
		writeError(c, errNotFound())
	})
	return r
}

// healthz 为存活探针：任一依赖检查失败返回 503。契约中该路径无响应体 schema，
// 因此只返回最少的状态字段，不套信封。
func healthz(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()

		failed := make([]string, 0, len(d.Checks))
		for name, check := range d.Checks {
			if err := check(ctx); err != nil {
				failed = append(failed, name)
			}
		}
		if len(failed) > 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "failed": failed})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func listPolicies(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		policies, err := d.Policies.ListPolicies(c.Request.Context())
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if policies == nil {
			policies = []Policy{}
		}
		writeOK(c, policies)
	}
}

func listDecisions(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "limit", Reason: err.Error()}))
			return
		}

		filter := DecisionFilter{}
		for field, raw := range map[string]string{
			"tenant_id": c.Query("tenant_id"),
			"policy_id": c.Query("policy_id"),
		} {
			if raw == "" {
				continue
			}
			if _, err := uuid.Parse(raw); err != nil {
				writeError(c, errInvalidArgument(ErrorDetail{Field: field, Reason: "必须是 UUID"}))
				return
			}
			if field == "tenant_id" {
				filter.TenantID = raw
			} else {
				filter.PolicyID = raw
			}
		}

		items, err := d.Decisions.ListDecisions(c.Request.Context(), filter)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}

		start := 0
		if raw := c.Query("cursor"); raw != "" {
			id, ok := decodeCursor(raw)
			if !ok {
				writeError(c, errInvalidArgument(ErrorDetail{Field: "cursor", Reason: "游标格式非法"}))
				return
			}
			idx := -1
			for i := range items {
				if items[i].ID == id {
					idx = i
					break
				}
			}
			if idx < 0 {
				writeError(c, errInvalidArgument(ErrorDetail{Field: "cursor", Reason: "游标已失效"}))
				return
			}
			start = idx + 1
		}

		rest := items[start:]
		page := decisionPage{Items: []PlacementDecision{}}
		if len(rest) > limit {
			page.Items = append(page.Items, rest[:limit]...)
			next := encodeCursor(rest[limit-1].ID)
			page.NextCursor = &next
		} else {
			page.Items = append(page.Items, rest...)
		}
		writeOK(c, page)
	}
}

func getDecision(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		decision, ok, err := d.Decisions.GetDecision(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if !ok {
			writeError(c, errNotFound())
			return
		}
		writeOK(c, decision)
	}
}

func parseLimit(raw string) (int, error) {
	if raw == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxLimit {
		return 0, errors.New("必须是 1..200 的整数")
	}
	return n, nil
}

func encodeCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeCursor(raw string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}
