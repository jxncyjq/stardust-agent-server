package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// 幽灵回归测试：父级撤销后，先前派生的子视图必须立即失效。
func TestSubsetViewLosesToolWhenParentRevokes(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	revoke := parent.Register("foo", okHandler())
	child := parent.Subset("foo")

	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "foo"}); err != nil {
		t.Fatalf("child Execute before revoke: %v", err)
	}

	revoke()

	_, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "foo"})
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("want ErrToolNotFound in child after parent revoke, got %v", err)
	}
}

func TestSubsetAllowListExcludesLaterUnlistedTools(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	parent.Register("foo", okHandler())
	child := parent.Subset("foo")

	parent.Register("bar", okHandler()) // 后注册且未列入 allow

	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "bar"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("allow-list must exclude later unlisted tools, got %v", err)
	}
}

func TestWithoutDenyListAdmitsLaterTools(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	parent.Register("foo", okHandler())
	child := parent.Without("foo")

	parent.Register("bar", okHandler()) // 后注册且未被 deny

	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "bar"}); err != nil {
		t.Fatalf("deny-only filter must admit later tools, got %v", err)
	}
	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "foo"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("denied tool must stay invisible, got %v", err)
	}
}

// 被过滤掉的工具，既不出现在 Descriptors 也拒绝执行——与不存在不可区分。
func TestFilteredToolIsInvisibleAndUnexecutable(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	parent.Register("foo", okHandler())
	parent.Register("secret", okHandler())
	child := parent.Subset("foo")

	for _, d := range child.Descriptors() {
		if d.Name == "secret" {
			t.Fatal("filtered tool must not appear in Descriptors")
		}
	}
	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "secret"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("filtered tool must refuse execution, got %v", err)
	}
}

// 作用域自有注册豁免过滤：委派出去的子作用域保留它自己应答的工具。
func TestOwnRegistrationBypassesFilter(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	parent.Register("foo", okHandler())
	child := parent.Subset("foo")
	child.Register("child_only", okHandler())

	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "child_only"}); err != nil {
		t.Fatalf("own registration must bypass the filter, got %v", err)
	}
	if _, err := parent.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "child_only"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("child registration must not leak upward, got %v", err)
	}
}

func TestNestedViewsIntersectFilters(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	parent.Register("a", okHandler())
	parent.Register("b", okHandler())
	grandchild := parent.Subset("a", "b").Without("b")

	if _, err := grandchild.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "a"}); err != nil {
		t.Fatalf("a must stay visible, got %v", err)
	}
	if _, err := grandchild.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "b"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("b must be denied by the nested filter, got %v", err)
	}
}
