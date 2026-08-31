package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func newEventRepo(t *testing.T) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// ev 造一个最小的合法事件；载荷内容与这些测试无关。
func ev(seq int64, typ domain.SessionEventType) domain.SessionEvent {
	return domain.SessionEvent{
		Seq:  seq,
		Type: typ,
		Time: time.UnixMilli(1_700_000_000_000 + seq),
		Data: []byte(`{}`),
	}
}

// 建表这一条不是形式主义：表名/主键写错的症状是「一切正常，直到两条事件撞了 seq」，
// 而那时错误信息指向的是 UNIQUE 约束，不是这次改动。
func TestTheSessionEventsTableExistsWithACompositeKey(t *testing.T) {
	ctx := context.Background()
	repo, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()

	var count int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_events'`,
	).Scan(&count); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if count != 1 {
		t.Fatalf("session_events 表不存在")
	}

	// 主键必须是 (session_id, seq) 两列：只按 seq 建键会让不同会话互相挤号。
	rows, err := repo.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('session_events') WHERE pk > 0 ORDER BY pk`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()
	var pk []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan pk column: %v", err)
		}
		pk = append(pk, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pk columns: %v", err)
	}
	if len(pk) != 2 || pk[0] != "session_id" || pk[1] != "seq" {
		t.Errorf("主键 = %v, want [session_id seq]", pk)
	}
}

// 首个 seq 必须等于 next-seq（spec §4.3 不变量 1）。
//
// 这条挡的是「两个写入方各按各的计数往里写」：一旦 seq 出现重叠或跳号，
// 日志就不再能重建出唯一的历史，而错误会在很久之后以「读的时候少一段」出现。
func TestAppendRefusesASeqThatDoesNotContinueTheLog(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	if err := repo.Append(ctx, "s1", []domain.SessionEvent{ev(0, domain.SessionEventTurnStart)}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// 库里的 next-seq 现在是 1；递交一个从 2 开始的批次必须被拒。
	err := repo.Append(ctx, "s1", []domain.SessionEvent{ev(2, domain.SessionEventStepStart)})
	if err == nil {
		t.Fatal("跳号的批次被接受了")
	}
	if !strings.Contains(err.Error(), "2") || !strings.Contains(err.Error(), "1") {
		t.Errorf("错误没同时给出实际与期望的 seq：%v", err)
	}
}

// 批中途失败要整批回滚（spec §4.3 不变量 1）。
//
// 半批写入留下的是一个「seq 连续但内容缺了后半段」的日志——它读得出来、也验得过，
// 却与真实发生的事不符。那种损坏比读不出来更难发现。
func TestAFailedBatchLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	// 第二条与第一条撞 seq：INSERT 到它时违反主键，整批必须回滚。
	batch := []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(0, domain.SessionEventUserMessage),
	}
	if err := repo.Append(ctx, "s1", batch); err == nil {
		t.Fatal("批内重复 seq 被接受了")
	}

	var remaining int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_events WHERE session_id = ?`, "s1").Scan(&remaining); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if remaining != 0 {
		t.Errorf("回滚之后还剩 %d 条事件：半批写入留下的日志读得出来却与真实发生的事不符", remaining)
	}
}

// 懒物化（spec §4.3 不变量 5）：没有事件的会话不在事件表里留任何痕迹。
func TestASessionWithNoEventsLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	if err := repo.Append(ctx, "s1", nil); err != nil {
		t.Fatalf("Append(nil) = %v, want nil：空批次是合法的无操作", err)
	}
	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_events`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Errorf("空批次写进了 %d 行", count)
	}
}

// 未知事件类型在写入时就被拒（不变量 4）：让它进库，读的那一方就只剩两个坏选择
// ——静默跳过（历史缺一段）或整条会话读不出来。
func TestAppendRefusesAnUnknownEventType(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	bad := domain.SessionEvent{Seq: 0, Type: "tool/telepathy", Time: time.UnixMilli(1), Data: []byte(`{}`)}
	err := repo.Append(ctx, "s1", []domain.SessionEvent{bad})
	if err == nil {
		t.Fatal("未知事件类型被写进库了")
	}
	if !strings.Contains(err.Error(), "tool/telepathy") {
		t.Errorf("错误没指名类型：%v", err)
	}
}

// 大载荷不进事件（不变量 6）：事件表的增长必须与**调用次数**成正比，
// 而不与工具输出体积成正比。超限的输出走 spill，事件里只留预览 + 定位符。
//
// 这条守在存储层，是为了让 P2 接发射点的人在**写错的当场**就看见，
// 而不是在库涨到几个 G 之后。
func TestAnOversizedPayloadIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	huge := make([]byte, maxSessionEventDataBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	payload, err := json.Marshal(map[string]string{"preview": string(huge)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = repo.Append(ctx, "s1", []domain.SessionEvent{{
		Seq: 0, Type: domain.SessionEventToolResult, Time: time.UnixMilli(1), Data: payload,
	}})
	if err == nil {
		t.Fatal("超限载荷被写进事件了：那会让事件表随工具输出体积膨胀")
	}
	if !strings.Contains(err.Error(), "spill") {
		t.Errorf("错误没告诉写的人该走 spill：%v", err)
	}
}
