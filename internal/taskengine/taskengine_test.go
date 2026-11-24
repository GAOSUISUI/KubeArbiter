package taskengine

import (
	"errors"
	"testing"
	"time"
)

func TestCheckTransition(t *testing.T) {
	allowed := []struct{ from, to Status }{
		{Pending, Queued},
		{Pending, PendingApproval},
		{Pending, Cancelled},
		{PendingApproval, Queued},
		{PendingApproval, Cancelled},
		{Queued, Running},
		{Queued, Cancelled},
		{Running, Succeeded},
		{Running, Failed},
		{Running, Retrying},
		{Retrying, Running},
		{Retrying, Failed},
		{Retrying, Cancelled},
	}
	for _, c := range allowed {
		if err := CheckTransition(c.from, c.to); err != nil {
			t.Errorf("%s -> %s 应允许: %v", c.from, c.to, err)
		}
	}

	denied := []struct{ from, to Status }{
		{Succeeded, Running},
		{Failed, Queued},
		{Cancelled, PendingApproval},
		{PendingApproval, Running},
		{Running, Cancelled},
		{Queued, Succeeded},
		{Pending, Succeeded},
	}
	for _, c := range denied {
		err := CheckTransition(c.from, c.to)
		if !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("%s -> %s 应拒绝, got %v", c.from, c.to, err)
		}
	}
}

func TestStatusHelpers(t *testing.T) {
	for _, s := range []Status{Pending, PendingApproval, Queued} {
		if !s.Cancellable() {
			t.Errorf("%s 应可取消", s)
		}
	}
	for _, s := range []Status{Running, Retrying, Succeeded, Failed, Cancelled} {
		if s.Cancellable() {
			t.Errorf("%s 不应可取消", s)
		}
	}
	for _, s := range []Status{Succeeded, Failed, Cancelled} {
		if !s.Terminal() {
			t.Errorf("%s 应为终态", s)
		}
	}
	if Pending.Terminal() {
		t.Error("PENDING 不是终态")
	}
}

func TestInitialStatus(t *testing.T) {
	cases := []struct {
		testWorkload, allowAuto bool
		want                    Status
	}{
		{false, false, PendingApproval},
		{false, true, PendingApproval},
		{true, false, PendingApproval},
		{true, true, Queued},
	}
	for _, c := range cases {
		if got := InitialStatus(c.testWorkload, c.allowAuto); got != c.want {
			t.Errorf("InitialStatus(%v,%v) = %s, want %s", c.testWorkload, c.allowAuto, got, c.want)
		}
	}
}

func TestCheckApprover(t *testing.T) {
	if err := CheckApprover("alice", "bob", false); err != nil {
		t.Errorf("不同审批人应通过: %v", err)
	}
	if err := CheckApprover("alice", "alice", false); err == nil {
		t.Error("创建人自审应拒绝")
	}
	if err := CheckApprover("alice", "alice", true); err != nil {
		t.Errorf("admin 自审应允许: %v", err)
	}
	if err := CheckApprover("alice", "", false); err == nil {
		t.Error("空审批人应拒绝")
	}
}

func TestRetryPolicy(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, BaseDelay: time.Second, MaxDelay: 5 * time.Second}

	delay, ok := p.Next(0, Retryable)
	if !ok || delay != time.Second {
		t.Errorf("第 1 次重试 = %v, %v", delay, ok)
	}
	if delay, ok = p.Next(1, Retryable); !ok || delay != 2*time.Second {
		t.Errorf("第 2 次重试 = %v, %v", delay, ok)
	}
	if _, ok = p.Next(3, Retryable); ok {
		t.Error("达到最大次数不应重试")
	}
	if _, ok = p.Next(0, Permanent); ok {
		t.Error("不可重试错误不应重试")
	}
	if delay, ok = p.Next(4, Retryable); ok || delay != 0 {
		t.Errorf("超限应返回 0,false, got %v,%v", delay, ok)
	}

	capped := RetryPolicy{MaxRetries: 10, BaseDelay: time.Second, MaxDelay: 5 * time.Second}
	if delay, _ := capped.Next(5, Retryable); delay != 5*time.Second {
		t.Errorf("退避应封顶 5s, got %v", delay)
	}
}
