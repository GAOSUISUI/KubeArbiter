package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const idempotencyHeader = "Idempotency-Key"

// IdempotentRecord 记录一次写请求的结果，用于同键重放。
type IdempotentRecord struct {
	Key         string
	Scope       string
	RequestHash string
	Status      int
	Body        []byte
}

// IdempotencyStore 持久化写接口的幂等结果。同键并发写入返回 ErrConflict，
// 调用方需回读先落库的记录并以它为准。
type IdempotencyStore interface {
	GetIdempotentRecord(ctx context.Context, key string) (IdempotentRecord, bool, error)
	SaveIdempotentRecord(ctx context.Context, record IdempotentRecord) error
}

// idempotencyKey 读取并校验 Idempotency-Key；为空表示本次请求不启用幂等。
func idempotencyKey(c *gin.Context) (string, *apiError) {
	key := strings.TrimSpace(c.GetHeader(idempotencyHeader))
	if key == "" {
		return "", nil
	}
	if len(key) > 128 {
		return "", errInvalidArgument(ErrorDetail{
			Field: idempotencyHeader, Reason: "长度不能超过 128",
		})
	}
	return key, nil
}

// requestFingerprint 以"方法 + 实际路径 + 请求体哈希"界定幂等作用域：
// 同键换个接口或换个请求体都属于键复用冲突。
func requestFingerprint(c *gin.Context, raw []byte) (scope, hash string) {
	scope = c.Request.Method + " " + c.Request.URL.Path
	sum := sha256.Sum256(raw)
	return scope, hex.EncodeToString(sum[:])
}

// runIdempotent 执行写操作并在成功后有条件地落库幂等记录。
// execute 返回完整响应体（已含 trace_id），以便重放时逐字节一致。
func runIdempotent(c *gin.Context, store IdempotencyStore, key, scope, hash string,
	execute func() (int, []byte, *apiError)) {
	ctx := c.Request.Context()

	if key == "" || store == nil {
		status, body, apiErr := execute()
		if apiErr != nil {
			writeError(c, apiErr)
			return
		}
		writeRaw(c, status, body)
		return
	}

	if key != "" {
		record, found, err := store.GetIdempotentRecord(ctx, key)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if found {
			replayOrConflict(c, record, scope, hash)
			return
		}
	}

	status, body, apiErr := execute()
	if apiErr != nil {
		// 并发同键请求可能已完成并落库；此时以先落库的结果为准，而不是报重复创建。
		if key != "" && apiErr.code == CodeIdempotencyConflict {
			if record, found, err := store.GetIdempotentRecord(ctx, key); err == nil && found {
				replayOrConflict(c, record, scope, hash)
				return
			}
		}
		writeError(c, apiErr)
		return
	}

	if key != "" {
		record := IdempotentRecord{Key: key, Scope: scope, RequestHash: hash, Status: status, Body: body}
		if err := store.SaveIdempotentRecord(ctx, record); err != nil {
			if errors.Is(err, ErrConflict) {
				if existing, found, getErr := store.GetIdempotentRecord(ctx, key); getErr == nil && found {
					replayOrConflict(c, existing, scope, hash)
					return
				}
			}
			// 写操作已提交，幂等缓存失败不回滚结果；客户端重试可能得到重复冲突，
			// 但不会造成数据错误（POST 有唯一键兜底，PUT/DELETE 天然幂等）。
		}
	}
	writeRaw(c, status, body)
}

func replayOrConflict(c *gin.Context, record IdempotentRecord, scope, hash string) {
	if record.Scope != scope || record.RequestHash != hash {
		writeError(c, errConflict("IDEMPOTENCY_KEY_REUSED", ErrorDetail{
			Field: idempotencyHeader, Reason: "同一键已用于不同的请求",
		}))
		return
	}
	writeRaw(c, record.Status, record.Body)
}

// writeRaw 输出已序列化的响应，重放与首次返回完全一致。
func writeRaw(c *gin.Context, status int, body []byte) {
	if status == http.StatusNoContent {
		c.Status(http.StatusNoContent)
		return
	}
	c.Data(status, "application/json; charset=utf-8", body)
}

// successBody 生成与 writeOK 完全一致的响应信封字节。
func successBody(c *gin.Context, data any) ([]byte, *apiError) {
	body, err := json.Marshal(envelope{Code: CodeOK, Data: data, TraceID: traceID(c)})
	if err != nil {
		return nil, errUnavailable()
	}
	return body, nil
}

// ---------- 内存实现 ----------

func (s *MemoryStore) GetIdempotentRecord(_ context.Context, key string) (IdempotentRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.idempotent[key]
	if !ok {
		return IdempotentRecord{}, false, nil
	}
	record.Body = append([]byte(nil), record.Body...)
	return record, true, nil
}

func (s *MemoryStore) SaveIdempotentRecord(_ context.Context, record IdempotentRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.idempotent[record.Key]; ok {
		return ErrConflict
	}
	record.Body = append([]byte(nil), record.Body...)
	s.idempotent[record.Key] = record
	return nil
}
