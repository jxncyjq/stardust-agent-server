package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/config"
)

// 内置浏览器至今**打不开内网**：browser.RuntimeConfig 有 AllowPrivateHosts 这个字段，
// 却没有任何配置键喂它，于是它恒为 false——用内置浏览器访问自家 staging、内网文档站
// 一律不可达，而配置文件里找不到任何解释。
//
// 现在给它一个键。默认仍是 false（SSRF 基础拦截是对的），但**打开它必须留痕**：
// 这是一台机器上「Agent 可以打内网」的开关，运维在事后翻日志时要看得见它开着。

func privateHostsConfig(t *testing.T, browserBlock string) string {
	t.Helper()

	dir := t.TempDir()
	path := dir + "/agent.json"
	body := `{"storage": {"driver": "memory"}, "context_files": {"root": ` +
		jsonString(dir) + `}, "browser": {` + browserBlock + `}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// buildWithLogger 起一个 serve 并把日志收进 buf，供断言那条 WARN。
func buildWithLogger(t *testing.T, configPath string) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := BuildServeService(ctx, ServeOptions{
		ConfigPath: configPath,
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(buf, nil)),
	})
	if err != nil {
		t.Fatalf("BuildServeService: %v", err)
	}
	t.Cleanup(result.Close)
	return buf
}

func TestAllowingPrivateHostsIsRecordedAtStartup(t *testing.T) {
	// browser.enabled 必须为 true，否则浏览器运行时根本不装配，这条 WARN 也就无从
	// 谈起——那也是对的：没有浏览器就没有这个风险。
	logs := buildWithLogger(t, privateHostsConfig(t, `"enabled": true, "allow_private_hosts": true`))

	text := logs.String()
	if !strings.Contains(text, "allow_private_hosts") {
		t.Errorf("startup logs never mention the switch:\n%s", text)
	}
	if !strings.Contains(text, "WARN") {
		t.Errorf("allowing the agent's browser onto the private network was not logged as a warning:\n%s", text)
	}
}

// TestTheDefaultStaysClosedAndQuiet：默认不放行，也不该每次启动都喊一句——一条永远
// 出现的告警等于没有告警。
func TestTheDefaultStaysClosedAndQuiet(t *testing.T) {
	logs := buildWithLogger(t, privateHostsConfig(t, `"enabled": true`))

	if strings.Contains(logs.String(), "allow_private_hosts") {
		t.Errorf("the default configuration warned about a switch nobody turned on:\n%s", logs.String())
	}
}

var _ = io.Discard

// TestEveryBrowserConfigKeyReachesTheRuntime 补上一条**只测日志测不到**的东西。
//
// 上一版只断言那条 WARN，于是把「把开关喂给运行时」那一行删掉，测试照样绿——
// 而那正是这个功能唯一起作用的地方。少接一个字段不会让任何东西报错：浏览器照常
// 起来，那个开关只是永远是零值。
func TestEveryBrowserConfigKeyReachesTheRuntime(t *testing.T) {
	t.Parallel()

	got := browserRuntimeConfig(config.BrowserConfig{
		Headless:              true,
		BinPath:               "/opt/chrome",
		AllowPrivateHosts:     true,
		RequireSandbox:        true,
		SessionTTLSeconds:     900,
		ReapIntervalSeconds:   120,
		MaxElements:           50,
		SnapshotRuneThreshold: 4000,
		SnapshotTTLHours:      48,
		SnapshotArchiveDir:    "snapshots",
		MinFreeMemoryMB:       512,
		MaxProcesses:          3,
		MaxContextsPerProcess: 4,
		ProcessMemoryLimitMB:  2048,
	})

	checks := []struct {
		name string
		ok   bool
	}{
		{"headless", got.Headless},
		{"bin_path", got.BinPath == "/opt/chrome"},
		{"allow_private_hosts", got.AllowPrivateHosts},
		{"require_sandbox", got.RequireSandbox},
		{"session_ttl_seconds", got.SessionTTL == 900*time.Second},
		{"reap_interval_seconds", got.ReapInterval == 120*time.Second},
		{"max_elements", got.MaxElements == 50},
		{"snapshot_rune_threshold", got.SnapshotRuneThreshold == 4000},
		{"snapshot_ttl_hours", got.SnapshotTTL == 48*time.Hour},
		{"snapshot_archive_dir", got.SnapshotArchiveDir == "snapshots"},
		{"min_free_memory_mb", got.MinFreeMemoryBytes == 512<<20},
		{"max_processes", got.MaxProcesses == 3},
		{"max_contexts_per_process", got.MaxContextsPerProcess == 4},
		{"process_memory_limit_mb", got.ProcessMemoryLimitBytes == 2048<<20},
	}
	for _, check := range checks {
		if !check.ok {
			t.Errorf("browser.%s never reaches the runtime; it was configured and silently ignored", check.name)
		}
	}
}
