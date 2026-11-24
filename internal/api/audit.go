package api

import (
	"context"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// AuditLog 对应 openapi.yaml 的 AuditLog。
type AuditLog struct {
	ID         int64          `json:"id"`
	Actor      string         `json:"actor"`
	Action     string         `json:"action"`
	ObjectType string         `json:"object_type"`
	ObjectID   *string        `json:"object_id"`
	Before     map[string]any `json:"before"`
	After      map[string]any `json:"after"`
	TraceID    *string        `json:"trace_id"`
	TS         time.Time      `json:"ts"`
}

type auditPage struct {
	NextCursor *string    `json:"next_cursor"`
	Items      []AuditLog `json:"items"`
}

// AuditFilter 为空字段表示不过滤。
type AuditFilter struct {
	ObjectType string
	ObjectID   string
}

// AuditReader 读取审计日志；审计表只追加，不提供更新与删除。
type AuditReader interface {
	ListAuditLogs(ctx context.Context, f AuditFilter) ([]AuditLog, error)
}

// AuditWriter 追加审计记录。
type AuditWriter interface {
	AppendAuditLog(ctx context.Context, log AuditLog) error
}

func listAuditLogs(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.AuditLogs == nil {
			writeError(c, errUnavailable())
			return
		}
		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "limit", Reason: err.Error()}))
			return
		}
		filter := AuditFilter{
			ObjectType: c.Query("object_type"),
			ObjectID:   c.Query("object_id"),
		}
		items, err := d.AuditLogs.ListAuditLogs(c.Request.Context(), filter)
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
				if formatAuditID(items[i].ID) == id {
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
		page := auditPage{Items: []AuditLog{}}
		if len(rest) > limit {
			page.Items = append(page.Items, rest[:limit]...)
			next := encodeCursor(formatAuditID(rest[limit-1].ID))
			page.NextCursor = &next
		} else {
			page.Items = append(page.Items, rest...)
		}
		writeOK(c, page)
	}
}

// recordAudit 追加一条审计；审计失败不阻断已完成的业务动作，
// 由数据库触发器与监控告警兜底（G6）。
func recordAudit(c *gin.Context, d Deps, action, objectType, objectID string, before, after map[string]any) {
	if d.AuditWriter == nil {
		return
	}
	var (
		objectIDPtr *string
		tracePtr    *string
	)
	if objectID != "" {
		objectIDPtr = &objectID
	}
	if tid := traceID(c); tid != "" {
		tracePtr = &tid
	}
	_ = d.AuditWriter.AppendAuditLog(c.Request.Context(), AuditLog{
		Actor:      actorOf(c),
		Action:     action,
		ObjectType: objectType,
		ObjectID:   objectIDPtr,
		Before:     before,
		After:      after,
		TraceID:    tracePtr,
		TS:         time.Now().UTC(),
	})
}

func formatAuditID(id int64) string {
	return strconv.FormatInt(id, 10)
}
