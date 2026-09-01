package storage

import (
	"context"
	"testing"
)

// 表必须真的没了。留着一张没人写的空表比删掉更糟：下一个人会以为它是真相源之一。
func TestTheConversationTurnsTableIsGone(t *testing.T) {
	repo := newEventRepo(t)
	for _, table := range []string{"conversation_turns", "conversation_turns_fts"} {
		var name string
		err := repo.db.QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE name = ?`, table).Scan(&name)
		if err == nil {
			t.Errorf("%s 还在：事件日志是唯一真相源（spec §3 取舍 A2），两个真相源迟早会漂移", table)
		}
	}
}
