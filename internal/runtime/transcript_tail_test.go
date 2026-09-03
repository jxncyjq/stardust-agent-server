package runtime

import (
	"testing"

	"github.com/stardust/legion-agent/internal/port"
)

// 一条会话历史的最后一条几乎总是**上一轮的收尾回答**——一条没有 tool_calls 的
// assistant。G3 把历史排在 message[0] 之后，于是整个请求以那条 assistant 结尾。
//
// 真机上这不是「模型可能顺着旧话题往下说」那种程度的问题，是**请求根本发不出去**：
//
//	openai chat endpoint returned 400 Bad Request:
//	{"error":{"message":"The `reasoning_content` in the thinking mode must be
//	passed back to the API.","type":"invalid_request_error"}}
//
// provider 把尾部 assistant 当成要续写的 prefill，thinking 模型于是要求这条
// assistant 必须带回它的 reasoning_content——而历史里的 assistant 永远没有。
// 凡是 default_profile 指向 thinking 系模型的部署（本仓样例的 dev/fast/review
// 都是），「G3 打开 + 恢复会话」就必然失败，不是偶发。
//
// 取证是一次受控对照：同一段 head + 同一批历史，唯一的差别是末尾多一条 user
// 消息——补上就通过并正确答出只存在于工具往返里的事实，不补就 400。
//
// 所以这条测试钉的是**尾部角色**，不是某个 provider 的错误码：请求必须以当前
// 任务的 user 消息收尾，模型最后读到的必须是「现在要做什么」。
func TestTheRequestDoesNotEndOnAStaleAssistantWhenHistoryIsATranscript(t *testing.T) {
	t.Parallel()

	on := runVolumeTask(t, true)
	if len(on.messages) == 0 {
		t.Fatal("模型一条消息都没收到：夹具没跑起来，下面的断言无意义")
	}

	// 先确认这次真的走了 transcript 那条路——否则「尾部不是 assistant」可能只是
	// 因为历史压根没进来，这条测试就变成了空过。
	if toolRoleChars(on.messages) <= 0 {
		t.Fatalf("打开 G3 后一条 tool 角色消息都没有：历史没以 transcript 进模型，尾部断言量不到东西")
	}

	last := on.messages[len(on.messages)-1]
	if last.Role == port.RoleAssistant {
		t.Errorf("请求以 assistant 结尾（内容 %q）：provider 会把它当 prefill 续写，"+
			"thinking 模型直接 400（reasoning_content must be passed back）。"+
			"当前任务必须排在历史之后收尾", truncateForMessage(last.Content))
	}
	if last.Role != port.RoleUser {
		t.Errorf("最后一条消息 role = %q，要 %q：模型最后读到的必须是当前任务",
			last.Role, port.RoleUser)
	}

	// 收尾那条必须**恰好**是当前任务的输入，而不是「含有」它。
	//
	// 这里不能用 strings.Contains：message[0] 的 header 本来就渲染了
	// "Input: 接着说"（cognitive/core.go），所以一个把整段 prompt 再发一遍的实现
	// ——请求体积直接翻倍——同样能让「含有」通过。等值断言才钉得住
	// appendCurrentInput 的契约：原样复述当前输入，一个字不多。
	if last.Content != "接着说" {
		t.Errorf("收尾的 user 消息 = %q，要恰好是当前任务输入「接着说」："+
			"多出来的内容意味着复述的不只是输入", truncateForMessage(last.Content))
	}
}

// truncateForMessage 只为让失败信息可读，不参与任何判定。
func truncateForMessage(s string) string {
	r := []rune(s)
	if len(r) <= 80 {
		return s
	}
	return string(r[:80]) + "…"
}
