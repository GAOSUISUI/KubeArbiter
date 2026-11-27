package aiops

import (
	"context"
	"time"
)

// Reader 读取事件列表与详情。
type Reader interface {
	ListIncidents(ctx context.Context, f Filter) ([]Incident, error)
	GetIncident(ctx context.Context, id string) (Detail, bool, error)
}

// Writer 负责事件的创建、严重度升级、恢复与追加诊断。
type Writer interface {
	CreateIncident(ctx context.Context, inc Incident, diag Diagnosis) (Incident, error)
	EscalateIncident(ctx context.Context, id string, severity Severity) error
	ResolveIncident(ctx context.Context, id string, at time.Time) error
	AppendDiagnosis(ctx context.Context, incidentID string, diag Diagnosis) (Diagnosis, error)
}

// Store 同时具备读写能力。
type Store interface {
	Reader
	Writer
}
