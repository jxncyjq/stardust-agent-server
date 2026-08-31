package cli

import (
	"bytes"
	"log/slog"
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
//
// 这些测试**不起浏览器**。上一版走的是完整的 BuildServeService，于是它在没有 bwrap
// 的 runner 上必然失败（Linux 侧的策略是「缺了就拒绝启动」）——agent-ci 因此连红四
// 次，而我每次只看了 Browser Matrix。配置的告警与映射不需要一个真的 Chromium 来证明。

func warningsFor(t *testing.T, cfg config.BrowserConfig) string {
	t.Helper()

	buf := &bytes.Buffer{}
	browserRuntimeConfig(cfg, slog.New(slog.NewTextHandler(buf, nil)))
	return buf.String()
}

func TestAllowingPrivateHostsIsRecordedAtStartup(t *testing.T) {
	t.Parallel()

	logs := warningsFor(t, config.BrowserConfig{Enabled: true, AllowPrivateHosts: true})

	if !strings.Contains(logs, "allow_private_hosts") {
		t.Errorf("startup logs never mention the switch:\n%s", logs)
	}
	if !strings.Contains(logs, "WARN") {
		t.Errorf("allowing the agent's browser onto the private network was not logged as a warning:\n%s", logs)
	}
}

// TestTheDefaultStaysClosedAndQuiet：默认不放行，也不该每次启动都喊一句——一条永远
// 出现的告警等于没有告警。
func TestTheDefaultStaysClosedAndQuiet(t *testing.T) {
	t.Parallel()

	logs := warningsFor(t, config.BrowserConfig{Enabled: true})

	if strings.Contains(logs, "allow_private_hosts") {
		t.Errorf("the default configuration warned about a switch nobody turned on:\n%s", logs)
	}
}

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
		BundledChromiumPath:   "/opt/app/chrome-bundled",
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
	}, nil)

	checks := []struct {
		name string
		ok   bool
	}{
		{"headless", got.Headless},
		{"bin_path", got.BinPath == "/opt/chrome"},
		{"bundled_chromium_path", got.BundledChromiumPath == "/opt/app/chrome-bundled"},
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

// 桌面 App 把一个固定版 Chromium 装在自己包里，而那个路径随安装位置变（.app 拖到
// 哪里都行）——配置文件说不出它，只有跑起来的宿主算得出来。所以 ServeOptions 上有
// 这个入口；而运维在配置里显式指名的浏览器不该被宿主的推断盖掉。

func TestTheEmbedderCanNameItsBundledBrowser(t *testing.T) {
	t.Parallel()

	got := browserRuntimeConfig(config.BrowserConfig{Enabled: true}, nil)
	if got.BundledChromiumPath != "" {
		t.Fatalf("BundledChromiumPath = %q from a config that names none", got.BundledChromiumPath)
	}
	// 装配那一跳的规则：配置为空才用宿主给的。
	if applyEmbedderBundle(got, "/Applications/Legion.app/Contents/Resources/chrome").
		BundledChromiumPath != "/Applications/Legion.app/Contents/Resources/chrome" {
		t.Error("the embedder's bundled browser never reaches the runtime; " +
			"a Chromium shipped inside the app stays invisible and the agent uses another one")
	}
}

func TestAConfiguredBundlePathOutranksTheEmbedders(t *testing.T) {
	t.Parallel()

	got := browserRuntimeConfig(config.BrowserConfig{
		Enabled: true, BundledChromiumPath: "/opt/pinned/chrome",
	}, nil)
	if applyEmbedderBundle(got, "/Applications/Legion.app/Contents/Resources/chrome").
		BundledChromiumPath != "/opt/pinned/chrome" {
		t.Error("the embedder's guess overwrote the browser the operator named explicitly")
	}
}
