package sessionstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这一组测试的由来值得记下来：`task_start` 的范围校验曾经在一次收尾里**根本没有落盘**
// （改动脚本中途失败，写入没执行），而 go build / go test 全绿、十个变异也全被抓住——
// 因为没有任何一条测试断言过「Load 会不会校验」。缺失的校验不会让任何既有断言变红。
//
// 所以这里直接钉住两条反序列化路径各自的行为，而不是钉 validateCheckpoint 这个函数：
// 校验若哪天又从某条路径上掉了，红的会是这里。

func writeCheckpointFile(t *testing.T, dir string, cp Checkpoint) string {
	t.Helper()
	sessionDir := SessionDir(dir, "sess-x")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(sessionDir, checkpointFileName)
	data, err := json.Marshal(cp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// 一个大过消息条数的 task_start 是磁盘上那份文件坏了，必须返回 error——不是 panic：
// RunTask 跑在没有 recover 覆盖的任务 goroutine 上，panic 会把一份坏文件放大成整个
// agent 进程崩溃，连带所有并发任务。
func TestLoadRejectsATaskStartPastTheEndOfMessages(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeCheckpointFile(t, root, Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		TaskID:        "t1",
		SessionKey:    "sess-x",
		Messages: []MessageSnapshot{
			{Role: "user", Content: "base"},
			{Role: "assistant", Content: "干活"},
		},
		TaskStart: 99,
	})

	store := NewStore(root)
	_, ok, err := store.Load("sess-x", "")
	if err == nil {
		t.Fatalf("越界的 task_start 被照收了（ok=%v）：这个下标会在续跑时的切片上 panic，"+
			"届时已经离坏掉的那份文件很远", ok)
	}
	if !strings.Contains(err.Error(), "task_start") {
		t.Errorf("错误信息里没提 task_start，定位不到坏在哪：%v", err)
	}
	if ok {
		t.Error("返回了 ok=true：调用方会拿一份没通过校验的检查点接着跑")
	}
}

// 没有任何消息的检查点是损坏的，而且它需要**自己那条**校验：范围检查放它过去
// （0 > 0 为假），于是一份空检查点会恢复成一个空 conversation，续跑时对 message[0]
// 取下标当场越界。写侧永远至少快照 message[0]（base prompt），所以空的不来自我们。
func TestLoadRejectsACheckpointWithNoMessages(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeCheckpointFile(t, root, Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		TaskID:        "t1",
		SessionKey:    "sess-x",
		Messages:      nil,
		TaskStart:     0,
	})

	store := NewStore(root)
	_, ok, err := store.Load("sess-x", "")
	if err == nil {
		t.Fatalf("一份没有消息的检查点被照收了（ok=%v）：续跑时会对 message[0] 越界", ok)
	}
	if !strings.Contains(err.Error(), "no messages") {
		t.Errorf("错误信息没说清是缺消息：%v", err)
	}
}

// 合法的 task_start 必须原样通过——校验不能把正常的检查点也挡掉。
func TestLoadAcceptsAValidTaskStart(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeCheckpointFile(t, root, Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		TaskID:        "t1",
		SessionKey:    "sess-x",
		Messages: []MessageSnapshot{
			{Role: "user", Content: "base"},
			{Role: "assistant", Content: "干活"},
		},
		TaskStart: 2, // 恰好等于条数：本任务的消息为空，合法
	})

	store := NewStore(root)
	cp, ok, err := store.Load("sess-x", "")
	if err != nil {
		t.Fatalf("合法的 task_start 被拒了：%v", err)
	}
	if !ok {
		t.Fatal("ok=false：检查点存在却没被读出来")
	}
	if cp.TaskStart != 2 {
		t.Errorf("读回的 task_start = %d，要 2", cp.TaskStart)
	}
}

// 字段缺席（老检查点）解码成 0，不是损坏——0 从来不是合法的 taskStart，消费方按 1 处理。
func TestLoadAcceptsACheckpointWithoutTaskStart(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	sessionDir := SessionDir(root, "sess-x")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// 手写 JSON：这正是本字段引入之前的二进制写下的形状，没有 task_start 键。
	raw := `{"schema_version":` + itoa(CheckpointSchemaVersion) + `,"task_id":"t1",` +
		`"session_key":"sess-x","messages":[{"role":"user","content":"base"}],` +
		`"pending_calls":[]}`
	if err := os.WriteFile(filepath.Join(sessionDir, checkpointFileName), []byte(raw), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := NewStore(root)
	cp, ok, err := store.Load("sess-x", "")
	if err != nil {
		t.Fatalf("没有 task_start 键的老检查点被拒了：%v", err)
	}
	if !ok {
		t.Fatal("ok=false")
	}
	if cp.TaskStart != 0 {
		t.Errorf("缺席的 task_start 解码成 %d，要 0（消费方据此按 1 处理）", cp.TaskStart)
	}
}

// ListSuspendedIn 是本包**第二条**反序列化路径。校验只落在 Load 上的话，启动期恢复
// 会读到未经校验的检查点——这正是把校验抽成共用 helper 的理由。
func TestListSuspendedInRejectsACorruptTaskStart(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeCheckpointFile(t, root, Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		TaskID:        "t1",
		SessionKey:    "sess-x",
		Messages:      []MessageSnapshot{{Role: "user", Content: "base"}},
		TaskStart:     42,
	})

	store := NewStore(root)
	_, err := store.ListSuspendedIn(root)
	if err == nil {
		t.Fatal("枚举挂起任务时照收了越界的 task_start：这条路径绕过了校验")
	}
	if !strings.Contains(err.Error(), "task_start") {
		t.Errorf("错误信息里没提 task_start：%v", err)
	}
}

// itoa 避免为一个数字引入 strconv 之外的依赖，也让上面的手写 JSON 保持可读。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
