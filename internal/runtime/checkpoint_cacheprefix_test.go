package runtime

import (
	"testing"

	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
)

// 检查点必须把 messages[0] 的缓存断点一起带回来。
//
// StablePrefixLen 只在 messages[0] 上有意义：adapter 据它给 provider 打
// cache_control 断点（internal/adapter/http_maas.go）。G3 把历史排在 messages[0]
// **之后**而不是之前，整个取舍的唯一理由就是保住这个断点——runtime.go 里那段注释
// 的原话是「历史插到前面会让每次请求都缓存未命中——G3 的代价本就是体积，再赔上
// 缓存不划算」。
//
// 而恢复路径把它买来的东西白白赔掉了：restoreConversation 之后再没有人调
// pinCachePrefix（全仓只有 fresh 路径调它），于是恢复出来的 messages[0] 带着零值，
// adapter 不打断点，**续跑的每一次请求都失去 prompt cache**。而「G3 打开 + 恢复
// 会话」恰恰是 G3 的主场景。
//
// checkpoint.go 自己的注释写着「这里是本包第二个构造 conversation 的地方，也是唯一
// 一个不经 newConversation/appendHistory 的——新增字段时最容易漏掉的正是它」。
// taskStart 刚走过这个坑，StablePrefixLen 是同一个坑里剩下的那个字段。
func TestACheckpointCarriesTheCachePrefixBoundary(t *testing.T) {
	t.Parallel()

	const prefixLen = 34
	convo := newConversation("stable prefix + volatile suffix", nil)
	convo.pinCachePrefix(prefixLen)
	convo.appendAssistant("干活", nil)

	if convo.messages[0].StablePrefixLen != prefixLen {
		t.Fatalf("夹具没搭对：pinCachePrefix 之后 messages[0].StablePrefixLen = %d，要 %d",
			convo.messages[0].StablePrefixLen, prefixLen)
	}

	restored := restoreConversation(snapshotMessages(convo), convo.taskStart)

	if got := restored.messages[0].StablePrefixLen; got != prefixLen {
		t.Errorf("恢复后 messages[0].StablePrefixLen = %d，要 %d："+
			"缓存断点没跟着检查点走，续跑的每一次请求都会失去 prompt cache——"+
			"而这正是 G3 把历史排在 messages[0] 之后所要保住的东西", got, prefixLen)
	}
}

// 断点只属于 messages[0]：抄回来时不能把它撒到后面的消息上，否则会在每任务都变的
// 内容里再打一个断点。
func TestRestoringDoesNotSpreadTheCachePrefixToOtherMessages(t *testing.T) {
	t.Parallel()

	convo := newConversation("base", nil)
	convo.pinCachePrefix(12)
	convo.appendAssistant("干活", nil)
	convo.appendUser("再来")

	restored := restoreConversation(snapshotMessages(convo), convo.taskStart)

	for i, msg := range restored.messages {
		if i == 0 {
			continue
		}
		if msg.StablePrefixLen != 0 {
			t.Errorf("恢复后 messages[%d].StablePrefixLen = %d，要 0："+
				"断点被撒到了 message[0] 以外，会在每任务都变的内容里再打一个缓存断点",
				i, msg.StablePrefixLen)
		}
	}
}

// 本字段引入之前写下的检查点没有这个键，解码得 0——与今天的行为相同，不需要迁移。
func TestACheckpointFromBeforeTheCachePrefixFieldRestoresToZero(t *testing.T) {
	t.Parallel()

	restored := restoreConversation([]sessionstate.MessageSnapshot{
		{Role: port.RoleUser, Content: "base"},
	}, 1)
	if got := restored.messages[0].StablePrefixLen; got != 0 {
		t.Errorf("老检查点恢复出的 StablePrefixLen = %d，要 0", got)
	}
}
