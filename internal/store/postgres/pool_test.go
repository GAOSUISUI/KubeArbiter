package postgres

import (
	"context"
	"strings"
	"testing"
)

func TestNewRejectsInvalidDSN(t *testing.T) {
	if _, err := New(context.Background(), "postgres://not a valid dsn"); err == nil {
		t.Fatal("非法 DSN 应返回错误")
	} else if !strings.Contains(err.Error(), "解析 DSN") {
		t.Fatalf("错误应标明解析阶段，实际: %v", err)
	}
}

func TestNewReportsConnectFailure(t *testing.T) {
	// 端口 1 上不会有 PostgreSQL 监听；连接应失败并带上 "连接 PostgreSQL" 前缀。
	_, err := New(context.Background(), "postgres://kubearbiter@127.0.0.1:1/kubearbiter?connect_timeout=1")
	if err == nil {
		t.Fatal("无法连接的 DSN 应返回错误")
	}
	if !strings.Contains(err.Error(), "连接 PostgreSQL") {
		t.Fatalf("错误应标明连接阶段，实际: %v", err)
	}
}
