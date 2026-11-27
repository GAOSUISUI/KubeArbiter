package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kubearbiter/kubearbiter/internal/aiops"
	"github.com/kubearbiter/kubearbiter/internal/slo"
)

var (
	_ aiops.Reader = (*PGStore)(nil)
	_ aiops.Writer = (*PGStore)(nil)
)

func (s *PGStore) ListIncidents(ctx context.Context, f aiops.Filter) ([]aiops.Incident, error) {
	var (
		where []string
		args  []any
	)
	add := func(column, value string) {
		args = append(args, value)
		where = append(where, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if f.Status != "" {
		add("status", f.Status)
	}
	if f.Type != "" {
		add("type", f.Type)
	}
	if f.Severity != "" {
		add("severity", f.Severity)
	}
	if f.Namespace != "" {
		add("namespace", f.Namespace)
	}
	if f.Target != "" {
		add("target", f.Target)
	}

	query := `SELECT id::text, target, namespace, type, severity, status, first_seen, resolved_at
		FROM incidents`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY first_seen DESC, id DESC"

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询事件: %w", err)
	}
	defer rows.Close()

	out := make([]aiops.Incident, 0)
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描事件: %w", err)
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

func (s *PGStore) GetIncident(ctx context.Context, id string) (aiops.Detail, bool, error) {
	if _, err := uuid.Parse(id); err != nil {
		return aiops.Detail{}, false, nil
	}
	inc, err := scanIncident(s.pool.QueryRow(ctx, `SELECT id::text, target, namespace, type, severity, status, first_seen, resolved_at
		FROM incidents WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return aiops.Detail{}, false, nil
	}
	if err != nil {
		return aiops.Detail{}, false, fmt.Errorf("查询事件: %w", err)
	}

	detail := aiops.Detail{Incident: inc, RecoveryActions: []aiops.RecoveryAction{}}
	diag, err := s.latestDiagnosis(ctx, id)
	if err != nil {
		return aiops.Detail{}, false, err
	}
	if diag != nil {
		detail.Diagnosis = diag
	}
	return detail, true, nil
}

func (s *PGStore) CreateIncident(ctx context.Context, inc aiops.Incident, diag aiops.Diagnosis) (aiops.Incident, error) {
	if inc.Status == "" {
		inc.Status = "OPEN"
	}
	if inc.FirstSeen.IsZero() {
		inc.FirstSeen = time.Now()
	}
	facts, err := json.Marshal(itemsDoc[aiops.Fact]{SchemaVersion: jsonSchemaVersion, Items: nonNilFacts(diag.Facts)})
	if err != nil {
		return aiops.Incident{}, fmt.Errorf("序列化诊断事实: %w", err)
	}
	inferences, err := json.Marshal(itemsDoc[aiops.Inference]{SchemaVersion: jsonSchemaVersion, Items: nonNilInferences(diag.Inferences)})
	if err != nil {
		return aiops.Incident{}, fmt.Errorf("序列化诊断推断: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return aiops.Incident{}, fmt.Errorf("开启事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := tx.QueryRow(ctx, `INSERT INTO incidents (target, namespace, type, severity, status, first_seen)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
		inc.Target, inc.Namespace, string(inc.Type), string(inc.Severity), inc.Status, inc.FirstSeen,
	).Scan(&inc.ID); err != nil {
		return aiops.Incident{}, fmt.Errorf("写入事件: %w", err)
	}

	diag.IncidentID = inc.ID
	if err := tx.QueryRow(ctx, `INSERT INTO diagnosis_reports (incident_id, facts, inferences)
		VALUES ($1::uuid, $2, $3) RETURNING id::text, created_at`,
		inc.ID, facts, inferences,
	).Scan(&diag.ID, &diag.CreatedAt); err != nil {
		return aiops.Incident{}, fmt.Errorf("写入诊断: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return aiops.Incident{}, fmt.Errorf("提交事务: %w", err)
	}
	return inc, nil
}

func (s *PGStore) EscalateIncident(ctx context.Context, id string, sev aiops.Severity) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE incidents SET severity = $2
		WHERE id = $1::uuid AND status = 'OPEN' AND severity <> $2`, id, string(sev))
	if err != nil {
		return fmt.Errorf("升级事件: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return nil
}

func (s *PGStore) ResolveIncident(ctx context.Context, id string, at time.Time) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	_, err := s.pool.Exec(ctx, `UPDATE incidents SET status = 'RESOLVED', resolved_at = $2
		WHERE id = $1::uuid AND status = 'OPEN'`, id, at)
	if err != nil {
		return fmt.Errorf("恢复事件: %w", err)
	}
	return nil
}

func (s *PGStore) AppendDiagnosis(ctx context.Context, incidentID string, diag aiops.Diagnosis) (aiops.Diagnosis, error) {
	if _, err := uuid.Parse(incidentID); err != nil {
		return aiops.Diagnosis{}, ErrNotFound
	}
	facts, err := json.Marshal(itemsDoc[aiops.Fact]{SchemaVersion: jsonSchemaVersion, Items: nonNilFacts(diag.Facts)})
	if err != nil {
		return aiops.Diagnosis{}, fmt.Errorf("序列化诊断事实: %w", err)
	}
	inferences, err := json.Marshal(itemsDoc[aiops.Inference]{SchemaVersion: jsonSchemaVersion, Items: nonNilInferences(diag.Inferences)})
	if err != nil {
		return aiops.Diagnosis{}, fmt.Errorf("序列化诊断推断: %w", err)
	}
	diag.IncidentID = incidentID
	if err := s.pool.QueryRow(ctx, `INSERT INTO diagnosis_reports (incident_id, facts, inferences)
		VALUES ($1::uuid, $2, $3) RETURNING id::text, created_at`,
		incidentID, facts, inferences,
	).Scan(&diag.ID, &diag.CreatedAt); err != nil {
		return aiops.Diagnosis{}, fmt.Errorf("写入诊断: %w", err)
	}
	return diag, nil
}

func (s *PGStore) latestDiagnosis(ctx context.Context, incidentID string) (*aiops.Diagnosis, error) {
	var (
		diag     aiops.Diagnosis
		factsRaw []byte
		inferRaw []byte
	)
	err := s.pool.QueryRow(ctx, `SELECT id::text, facts, inferences, created_at
		FROM diagnosis_reports WHERE incident_id = $1::uuid ORDER BY created_at DESC LIMIT 1`, incidentID,
	).Scan(&diag.ID, &factsRaw, &inferRaw, &diag.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询诊断: %w", err)
	}
	diag.IncidentID = incidentID
	if err := decodeItems(factsRaw, &diag.Facts); err != nil {
		return nil, err
	}
	if err := decodeItems(inferRaw, &diag.Inferences); err != nil {
		return nil, err
	}
	return &diag, nil
}

func (s *PGStore) AppendSLOBucket(ctx context.Context, service string, bucket slo.Bucket) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO slo_windows (service, window_start, total, errors, latency_ok)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (service, window_start) DO NOTHING`,
		service, bucket.Start, bucket.Total, bucket.Errors, bucket.LatencyOK)
	if err != nil {
		return fmt.Errorf("写入 SLO 窗口: %w", err)
	}
	return nil
}

func scanIncident(row scannable) (aiops.Incident, error) {
	var (
		inc aiops.Incident
		typ string
		sev string
	)
	if err := row.Scan(&inc.ID, &inc.Target, &inc.Namespace, &typ, &sev, &inc.Status, &inc.FirstSeen, &inc.ResolvedAt); err != nil {
		return aiops.Incident{}, err
	}
	inc.Type = aiops.IncidentType(typ)
	inc.Severity = aiops.Severity(sev)
	return inc, nil
}

func nonNilFacts(items []aiops.Fact) []aiops.Fact {
	if items == nil {
		return []aiops.Fact{}
	}
	return items
}

func nonNilInferences(items []aiops.Inference) []aiops.Inference {
	if items == nil {
		return []aiops.Inference{}
	}
	return items
}
