package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 默认必须是关。spec §3：G3 改的是每次请求的体积，「不该在做轨迹的顺路上悄悄打开」。
func TestLoadToolTranscriptDefaultsToFalse(t *testing.T) {
	// 不 parallel：t.Setenv 把这条测试与运维自己导出的同名变量隔开。
	t.Setenv("LEGION_AGENT_TOOL_TRANSCRIPT", "")
	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load(defaults) error = %v, want nil", err)
	}
	if cfg.Session.ToolTranscriptEnabled {
		t.Fatalf("Load(defaults).Session.ToolTranscriptEnabled = %t, want false",
			cfg.Session.ToolTranscriptEnabled)
	}
}

func TestLoadToolTranscriptFromJSONFile(t *testing.T) {
	t.Setenv("LEGION_AGENT_TOOL_TRANSCRIPT", "")
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{"session": {"tool_transcript_enabled": true}}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}
	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if !cfg.Session.ToolTranscriptEnabled {
		t.Fatal("配置文件里写了 tool_transcript_enabled: true 却没生效")
	}
}

func TestLoadToolTranscriptFromEnvironment(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"true", true}, {"1", true}, {"TRUE", true},
		{"false", false}, {"0", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("LEGION_AGENT_TOOL_TRANSCRIPT", tc.value)
			cfg, err := Load(context.Background(), Options{})
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if cfg.Session.ToolTranscriptEnabled != tc.want {
				t.Errorf("LEGION_AGENT_TOOL_TRANSCRIPT=%q → %t，want %t",
					tc.value, cfg.Session.ToolTranscriptEnabled, tc.want)
			}
		})
	}
}

// 环境变量能覆盖配置文件里写的值——否则容器部署改一个开关就得重新打配置文件。
func TestToolTranscriptEnvironmentOverridesTheFile(t *testing.T) {
	t.Setenv("LEGION_AGENT_TOOL_TRANSCRIPT", "true")
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{"session": {"tool_transcript_enabled": false}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Session.ToolTranscriptEnabled {
		t.Error("环境变量没能覆盖配置文件里的 false")
	}
}

// 不可解析的值必须 fail-loud，不能静默落回 false。
//
// 这一条与上面那些便利开关（`== "true" || == "1"`）不同，走的是 REQUIRE_IDENTITY
// 那一档：写 TOOL_TRANSCRIPT=yes 的人是想打开它，静默落回 false 会让他拿到零效果加
// 零警告——而「请求体积没变」恰恰最容易被读成「这开关没用」。
func TestToolTranscriptRejectsAnUnparseableValue(t *testing.T) {
	t.Setenv("LEGION_AGENT_TOOL_TRANSCRIPT", "yes")
	_, err := Load(context.Background(), Options{})
	if err == nil {
		t.Fatal("LEGION_AGENT_TOOL_TRANSCRIPT=yes 被静默忽略了：" +
			"运维会以为开关生效，而请求体积没变会被读成「这开关没用」")
	}
	if !strings.Contains(err.Error(), "LEGION_AGENT_TOOL_TRANSCRIPT") {
		t.Errorf("错误信息里没提是哪个变量：%v", err)
	}
}

// 示例配置必须列出 SessionConfig 的**每一个** json 字段。
//
// 这条测试的由来：`tool_transcript_enabled` 加进 SessionConfig 之后，两个示例文件
// 都没跟着更新，`cache_enabled` / `cache_max_entries` 也一直缺着——不读代码的人无从
// 知道这些开关存在。示例与结构体的同步靠人记是记不住的，所以让它变成一条断言。
func TestTheExampleConfigsListEverySessionField(t *testing.T) {
	t.Parallel()

	var want []string
	rt := reflect.TypeOf(SessionConfig{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		want = append(want, strings.Split(tag, ",")[0])
	}
	if len(want) == 0 {
		t.Fatal("SessionConfig 没有带 json tag 的字段：这条测试量不到东西")
	}

	for _, file := range []string{
		"../../configs/agent.complete.example.json",
		"../../configs/agent.full.example.json",
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("读 %s：%v", file, err)
		}
		var doc struct {
			Session map[string]json.RawMessage `json:"session"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("解析 %s：%v", file, err)
		}
		if len(doc.Session) == 0 {
			t.Fatalf("%s 里没有 session 段", file)
		}
		for _, key := range want {
			if _, ok := doc.Session[key]; !ok {
				t.Errorf("%s 的 session 段缺 %q：不读代码的人不会知道有这个开关",
					filepath.Base(file), key)
			}
		}
	}
}
