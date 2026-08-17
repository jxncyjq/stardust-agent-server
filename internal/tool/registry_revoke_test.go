package tool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func okHandler() Handler {
	return HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Output: "ok"}, nil
	})
}

func TestRegisterReturnsWorkingRevoke(t *testing.T) {
	r := NewRegistry(nil, nil, nil)
	revoke := r.Register("echo", okHandler())

	if _, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "echo"}); err != nil {
		t.Fatalf("Execute before revoke: %v", err)
	}

	revoke()

	_, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "echo"})
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("want ErrToolNotFound after revoke, got %v", err)
	}
	for _, d := range r.Descriptors() {
		if d.Name == "echo" {
			t.Fatal("revoked tool must not appear in Descriptors")
		}
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	r := NewRegistry(nil, nil, nil)
	revoke := r.Register("echo", okHandler())
	revoke()
	revoke()
	r.Register("echo", okHandler()) // name must be free again
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic on duplicate tool name")
		}
	}()
	r := NewRegistry(nil, nil, nil)
	r.Register("echo", okHandler())
	r.Register("echo", okHandler())
}

func TestReplaceOverridesAndRestoresNothing(t *testing.T) {
	r := NewRegistry(nil, nil, nil)
	r.Register("echo", okHandler())
	revoke := r.Replace(Descriptor{Name: "echo"}, HandlerFunc(
		func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Output: "replaced"}, nil
		}))

	res, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "echo"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output != "replaced" {
		t.Fatalf("want replaced handler, got %q", res.Output)
	}

	revoke()
	if _, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "echo"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Replace revoke removes the name outright, got %v", err)
	}
}

func TestConcurrentRegisterRevokeExecute(t *testing.T) {
	r := NewRegistry(nil, nil, nil)
	r.Register("stable", okHandler())

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "stable"})
		}()
		// 每个 goroutine 用唯一名：重名注册是 fail-loud 的 panic，
		// 并发测试要压的是锁，不是重名契约。
		go func(i int) {
			defer wg.Done()
			revoke := r.Register(fmt.Sprintf("churn-%d", i), okHandler())
			revoke()
		}(i)
	}
	wg.Wait()
}
