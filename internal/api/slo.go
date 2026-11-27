package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kubearbiter/kubearbiter/internal/slo"
)

// SLOReader 读取 SLO 定义与 1 分钟预聚合窗口。实现方只做 IO，
// 预算与燃尽率计算全部留在 internal/slo，保证接口与服务端口径一致。
type SLOReader interface {
	ListSLODefinitions(ctx context.Context) ([]slo.Definition, error)
	GetSLODefinition(ctx context.Context, service string) (slo.Definition, bool, error)
	ListSLOBuckets(ctx context.Context, service string, from, to time.Time) ([]slo.Bucket, error)
}

// SLODefinitionWriter 提供 SLO 定义 CRUD，service + sli_type 为唯一键。
// 创建冲突返回 ErrConflict，更新/删除不存在返回 ErrNotFound。
type SLODefinitionWriter interface {
	CreateSLODefinition(ctx context.Context, def slo.Definition) (slo.Definition, error)
	UpdateSLODefinition(ctx context.Context, def slo.Definition) (slo.Definition, error)
	DeleteSLODefinition(ctx context.Context, service string, sli slo.SLIType) error
}

// sloStatus 对应 openapi.yaml 的 SLOStatus。
type sloStatus struct {
	Service           string      `json:"service"`
	SLIType           slo.SLIType `json:"sli_type"`
	Target            float64     `json:"target"`
	Current           float64     `json:"current"`
	BudgetRemaining   float64     `json:"budget_remaining"`
	TotalRequests     int64       `json:"total_requests"`
	PublishingAllowed bool        `json:"publishing_allowed"`
	Degraded          bool        `json:"degraded"`
}

// burnRateWindow 对应 openapi.yaml 的 BurnRateWindow。
type burnRateWindow struct {
	LongWindow   string  `json:"long_window"`
	ShortWindow  string  `json:"short_window"`
	Threshold    float64 `json:"threshold"`
	BurnRate     float64 `json:"burn_rate"`
	Firing       bool    `json:"firing"`
	Completeness float64 `json:"completeness"`
}

// sloDetailResp 对应 openapi.yaml 的 SLODetail。
type sloDetailResp struct {
	Service            string           `json:"service"`
	SLIType            slo.SLIType      `json:"sli_type"`
	Target             float64          `json:"target"`
	LatencyThresholdMS *int             `json:"latency_threshold_ms"`
	Windows            []burnRateWindow `json:"windows"`
	Degraded           bool             `json:"degraded"`
}

// budgetStatus 对应 openapi.yaml 的 BudgetStatus，额外携带治理动作。
type budgetStatus struct {
	Service           string     `json:"service"`
	Window            string     `json:"window"`
	BudgetTotal       float64    `json:"budget_total"`
	BudgetConsumed    float64    `json:"budget_consumed"`
	BudgetRemaining   float64    `json:"budget_remaining"`
	TotalRequests     int64      `json:"total_requests"`
	PublishingAllowed bool       `json:"publishing_allowed"`
	Degraded          bool       `json:"degraded"`
	Completeness      float64    `json:"completeness"`
	Action            slo.Action `json:"action"`
	Reasons           []string   `json:"reasons,omitempty"`
}

func listSLO(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.SLO == nil {
			writeError(c, errUnavailable())
			return
		}
		ctx := c.Request.Context()
		defs, err := d.SLO.ListSLODefinitions(ctx)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		now := time.Now().UTC()
		out := make([]sloStatus, 0, len(defs))
		for _, def := range defs {
			buckets, err := d.SLO.ListSLOBuckets(ctx, def.Service, lookbackStart(now, lookback(def.Window)), now)
			if err != nil {
				writeError(c, errUnavailable())
				return
			}
			report := slo.Evaluate(def, buckets, now)
			decision := slo.Decide(report, 0)
			out = append(out, sloStatus{
				Service:           def.Service,
				SLIType:           def.SLIType,
				Target:            def.Target,
				Current:           report.Budget.Current,
				BudgetRemaining:   report.Budget.BudgetRemaining,
				TotalRequests:     report.Budget.TotalRequests,
				PublishingAllowed: decision.PublishingAllowed,
				Degraded:          report.Budget.Degraded,
			})
		}
		writeOK(c, out)
	}
}

func getSLODetail(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.SLO == nil {
			writeError(c, errUnavailable())
			return
		}
		ctx := c.Request.Context()
		def, ok, err := d.SLO.GetSLODefinition(ctx, c.Param("service"))
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if !ok {
			writeError(c, errNotFound())
			return
		}

		now := time.Now().UTC()
		buckets, err := d.SLO.ListSLOBuckets(ctx, def.Service, lookbackStart(now, slo.LongestWindow()), now)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		windows := slo.EvaluateWindows(def, buckets, now)
		degraded := false
		items := make([]burnRateWindow, 0, len(windows))
		for _, w := range windows {
			if w.Completeness < slo.MinCompleteness {
				degraded = true
			}
			items = append(items, burnRateWindow{
				LongWindow:   w.LongWindow,
				ShortWindow:  w.ShortWindow,
				Threshold:    w.Threshold,
				BurnRate:     w.BurnRate,
				Firing:       w.Firing,
				Completeness: w.Completeness,
			})
		}
		writeOK(c, sloDetailResp{
			Service:            def.Service,
			SLIType:            def.SLIType,
			Target:             def.Target,
			LatencyThresholdMS: def.LatencyThresholdMS,
			Windows:            items,
			Degraded:           degraded,
		})
	}
}

func getSLOBudget(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.SLO == nil {
			writeError(c, errUnavailable())
			return
		}
		ctx := c.Request.Context()
		def, ok, err := d.SLO.GetSLODefinition(ctx, c.Param("service"))
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if !ok {
			writeError(c, errNotFound())
			return
		}

		window := def.Window
		if raw := c.Query("window"); raw != "" {
			parsed, ok := slo.ParseWindow(raw)
			if !ok {
				writeError(c, errInvalidArgument(ErrorDetail{
					Field: "window", Reason: "必须是 1h、6h、3d 或 30d",
				}))
				return
			}
			window = parsed
		}

		now := time.Now().UTC()
		buckets, err := d.SLO.ListSLOBuckets(ctx, def.Service, lookbackStart(now, lookback(window)), now)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		budget := slo.ComputeBudget(def, buckets, now, window)
		decision := slo.Decide(slo.Report{
			Definition: def,
			Budget:     budget,
			Windows:    slo.EvaluateWindows(def, buckets, now),
		}, 0)

		writeOK(c, budgetStatus{
			Service:           def.Service,
			Window:            slo.FormatWindow(window),
			BudgetTotal:       budget.BudgetTotal,
			BudgetConsumed:    budget.BudgetConsumed,
			BudgetRemaining:   budget.BudgetRemaining,
			TotalRequests:     budget.TotalRequests,
			PublishingAllowed: decision.PublishingAllowed,
			Degraded:          budget.Degraded,
			Completeness:      budget.Completeness,
			Action:            decision.Action,
			Reasons:           decision.Reasons,
		})
	}
}

// lookback 保证读取范围同时覆盖预算窗口与最长的燃尽率窗口。
func lookback(budgetWindow time.Duration) time.Duration {
	if longest := slo.LongestWindow(); longest > budgetWindow {
		return longest
	}
	return budgetWindow
}

// lookbackStart 把起点对齐到整分钟，避免漏读窗口首分钟的预聚合桶。
func lookbackStart(now time.Time, window time.Duration) time.Time {
	return now.Add(-window).Truncate(time.Minute)
}

// sloDefinitionResp 对应 openapi.yaml 的 SLODefinition。
type sloDefinitionResp struct {
	Service            string      `json:"service"`
	SLIType            slo.SLIType `json:"sli_type"`
	Target             float64     `json:"target"`
	WindowSeconds      int64       `json:"window_seconds"`
	LatencyThresholdMS *int        `json:"latency_threshold_ms"`
}

// sloDefinitionRequest 是创建请求体；更新时 service 与 sli_type 由路径决定。
type sloDefinitionRequest struct {
	Service            string  `json:"service"`
	SLIType            string  `json:"sli_type"`
	Target             float64 `json:"target"`
	WindowSeconds      int64   `json:"window_seconds"`
	LatencyThresholdMS *int    `json:"latency_threshold_ms"`
}

// sloUpdateRequest 只允许修改数值字段，唯一键不可变。
type sloUpdateRequest struct {
	Target             float64 `json:"target"`
	WindowSeconds      int64   `json:"window_seconds"`
	LatencyThresholdMS *int    `json:"latency_threshold_ms"`
}

func (r sloDefinitionRequest) definition() slo.Definition {
	return slo.Definition{
		Service:            r.Service,
		SLIType:            slo.SLIType(r.SLIType),
		Target:             r.Target,
		Window:             time.Duration(r.WindowSeconds) * time.Second,
		LatencyThresholdMS: r.LatencyThresholdMS,
	}
}

func (r sloUpdateRequest) definition(service string, sli slo.SLIType) slo.Definition {
	return slo.Definition{
		Service:            service,
		SLIType:            sli,
		Target:             r.Target,
		Window:             time.Duration(r.WindowSeconds) * time.Second,
		LatencyThresholdMS: r.LatencyThresholdMS,
	}
}

// validateSLODefinition 把 field 级错误直接映射到 ErrorDetail，避免前端只看到一句话。
func validateSLODefinition(def slo.Definition) *apiError {
	switch {
	case def.Service == "":
		return errInvalidArgument(ErrorDetail{Field: "service", Reason: "必须非空"})
	case len(def.Service) > 253:
		return errInvalidArgument(ErrorDetail{Field: "service", Reason: "长度不能超过 253"})
	case def.SLIType.Validate() != nil:
		return errInvalidArgument(ErrorDetail{Field: "sli_type", Reason: "必须是 AVAILABILITY 或 LATENCY"})
	case def.Target <= 0 || def.Target >= 1:
		return errInvalidArgument(ErrorDetail{Field: "target", Reason: "必须在 (0,1) 之间"})
	case def.Window <= 0:
		return errInvalidArgument(ErrorDetail{Field: "window_seconds", Reason: "必须大于 0"})
	case def.Window > 10*365*24*time.Hour:
		return errInvalidArgument(ErrorDetail{Field: "window_seconds", Reason: "不能超过 10 年"})
	case def.SLIType == slo.SLILatency && (def.LatencyThresholdMS == nil || *def.LatencyThresholdMS <= 0):
		return errInvalidArgument(ErrorDetail{Field: "latency_threshold_ms", Reason: "LATENCY 类型必须提供正值"})
	case def.SLIType == slo.SLIAvailability && def.LatencyThresholdMS != nil:
		return errInvalidArgument(ErrorDetail{Field: "latency_threshold_ms", Reason: "仅 LATENCY 类型允许设置"})
	default:
		return nil
	}
}

func definitionResponse(def slo.Definition) sloDefinitionResp {
	return sloDefinitionResp{
		Service:            def.Service,
		SLIType:            def.SLIType,
		Target:             def.Target,
		WindowSeconds:      int64(def.Window / time.Second),
		LatencyThresholdMS: def.LatencyThresholdMS,
	}
}

func createSLODefinition(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.SLOWriter == nil {
			writeError(c, errUnavailable())
			return
		}
		raw, err := c.GetRawData()
		if err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "body", Reason: err.Error()}))
			return
		}
		key, apiErr := idempotencyKey(c)
		if apiErr != nil {
			writeError(c, apiErr)
			return
		}
		scope, hash := requestFingerprint(c, raw)
		runIdempotent(c, d.Idempotency, key, scope, hash, func() (int, []byte, *apiError) {
			return createSLO(c, d, raw)
		})
	}
}

func createSLO(c *gin.Context, d Deps, raw []byte) (int, []byte, *apiError) {
	var req sloDefinitionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0, nil, errInvalidArgument(ErrorDetail{Field: "body", Reason: err.Error()})
	}
	def := req.definition()
	if apiErr := validateSLODefinition(def); apiErr != nil {
		return 0, nil, apiErr
	}
	created, err := d.SLOWriter.CreateSLODefinition(c.Request.Context(), def)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return 0, nil, errConflict("SLO_DEFINITION_EXISTS",
				ErrorDetail{Field: "service", Reason: "同一 service + sli_type 已存在"})
		}
		return 0, nil, errUnavailable()
	}
	body, apiErr := successBody(c, definitionResponse(created))
	if apiErr != nil {
		return 0, nil, apiErr
	}
	return http.StatusOK, body, nil
}

func updateSLODefinition(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.SLOWriter == nil {
			writeError(c, errUnavailable())
			return
		}
		raw, err := c.GetRawData()
		if err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "body", Reason: err.Error()}))
			return
		}
		key, apiErr := idempotencyKey(c)
		if apiErr != nil {
			writeError(c, apiErr)
			return
		}
		scope, hash := requestFingerprint(c, raw)
		runIdempotent(c, d.Idempotency, key, scope, hash, func() (int, []byte, *apiError) {
			return updateSLO(c, d, raw)
		})
	}
}

func updateSLO(c *gin.Context, d Deps, raw []byte) (int, []byte, *apiError) {
	sli := slo.SLIType(c.Param("sliType"))
	if sli.Validate() != nil {
		return 0, nil, errInvalidArgument(ErrorDetail{Field: "sli_type", Reason: "必须是 AVAILABILITY 或 LATENCY"})
	}
	var req sloUpdateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0, nil, errInvalidArgument(ErrorDetail{Field: "body", Reason: err.Error()})
	}
	def := req.definition(c.Param("service"), sli)
	if apiErr := validateSLODefinition(def); apiErr != nil {
		return 0, nil, apiErr
	}
	updated, err := d.SLOWriter.UpdateSLODefinition(c.Request.Context(), def)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil, errNotFound()
		}
		return 0, nil, errUnavailable()
	}
	body, apiErr := successBody(c, definitionResponse(updated))
	if apiErr != nil {
		return 0, nil, apiErr
	}
	return http.StatusOK, body, nil
}

func deleteSLODefinition(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.SLOWriter == nil {
			writeError(c, errUnavailable())
			return
		}
		key, apiErr := idempotencyKey(c)
		if apiErr != nil {
			writeError(c, apiErr)
			return
		}
		scope, hash := requestFingerprint(c, nil)
		runIdempotent(c, d.Idempotency, key, scope, hash, func() (int, []byte, *apiError) {
			return deleteSLO(c, d)
		})
	}
}

func deleteSLO(c *gin.Context, d Deps) (int, []byte, *apiError) {
	sli := slo.SLIType(c.Param("sliType"))
	if sli.Validate() != nil {
		return 0, nil, errInvalidArgument(ErrorDetail{Field: "sli_type", Reason: "必须是 AVAILABILITY 或 LATENCY"})
	}
	if err := d.SLOWriter.DeleteSLODefinition(c.Request.Context(), c.Param("service"), sli); err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil, errNotFound()
		}
		return 0, nil, errUnavailable()
	}
	return http.StatusNoContent, nil, nil
}

// ---------- 内存实现（单测与无数据库模式） ----------

func (s *MemoryStore) AddSLODefinition(def slo.Definition) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sloDefs[sloKey(def.Service, def.SLIType)] = def
}

func (s *MemoryStore) AddSLOBucket(service string, bucket slo.Bucket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sloBuckets[service] = append(s.sloBuckets[service], bucket)
}

func (s *MemoryStore) CreateSLODefinition(_ context.Context, def slo.Definition) (slo.Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sloKey(def.Service, def.SLIType)
	if _, ok := s.sloDefs[key]; ok {
		return slo.Definition{}, ErrConflict
	}
	s.sloDefs[key] = def
	return def, nil
}

func (s *MemoryStore) UpdateSLODefinition(_ context.Context, def slo.Definition) (slo.Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sloKey(def.Service, def.SLIType)
	if _, ok := s.sloDefs[key]; !ok {
		return slo.Definition{}, ErrNotFound
	}
	s.sloDefs[key] = def
	return def, nil
}

func (s *MemoryStore) DeleteSLODefinition(_ context.Context, service string, sli slo.SLIType) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sloKey(service, sli)
	if _, ok := s.sloDefs[key]; !ok {
		return ErrNotFound
	}
	delete(s.sloDefs, key)
	return nil
}

func (s *MemoryStore) ListSLODefinitions(_ context.Context) ([]slo.Definition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]slo.Definition, 0, len(s.sloDefs))
	for _, def := range s.sloDefs {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].SLIType < out[j].SLIType
	})
	return out, nil
}

// GetSLODefinition 同服务存在可用性与延迟两个定义时优先返回可用性，
// 与契约 SLODetail 只有单个 sli_type 的限制保持一致。
func (s *MemoryStore) GetSLODefinition(_ context.Context, service string) (slo.Definition, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if def, ok := s.sloDefs[sloKey(service, slo.SLIAvailability)]; ok {
		return def, true, nil
	}
	def, ok := s.sloDefs[sloKey(service, slo.SLILatency)]
	return def, ok, nil
}

func (s *MemoryStore) ListSLOBuckets(_ context.Context, service string, from, to time.Time) ([]slo.Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]slo.Bucket, 0, len(s.sloBuckets[service]))
	for _, bucket := range s.sloBuckets[service] {
		if bucket.Start.Before(from) || !bucket.Start.Before(to) {
			continue
		}
		out = append(out, bucket)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func sloKey(service string, sli slo.SLIType) string {
	return service + "\x00" + string(sli)
}
