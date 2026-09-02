package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// spec §7 的「详情读取」：tool/result 只有预览 + 定位符，展开时按定位符取全文。
// 这条测试钉的是**两个根真的同源**——spill 写在 toolRoot 下（runtime.Config.ToolRoot，
// 由 agentToolRoot 解析成 task.WorkingDir），/v1/files 读的是 session.WorkingDir，
// 而任务的 WorkingDir 继承自会话（http.go 的 taskWorkingDir = session.WorkingDir）。
// spec 原话：「能否直接被它服务需在实现时确认——根不同源就要补」。
//
// 它断言的是端到端的结果（HTTP 真的把全文吐出来了），不是「路径字符串看起来一样」：
// 会话的 WorkingDir 就是写 spill 文件的那个目录，定位符是相对它的路径，请求原样交给
// /v1/files。把 WorkingDir 换成别的目录，这条测试必须变红——那正是它验的关系。
//
// 定位符的**形状**（.stardust/tool_results/<会话>/<工具>-<hash>.md）由 runtime 包的
// 测试钉住（TestRunTaskRecordsASpillLocatorThatNamesARealFile 拿事件里的定位符去工具
// 根下 Stat）；这里刻意用同样形状的真实路径，因为其中的点号开头目录与多级嵌套是
// /v1/files 的路径解析必须能吃下的部分。
func TestASpillLocatorCanBeServedByTheFilesEndpoint(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	// 造一个 spill 文件，位置与 writeToolResultCache 的产物一致：toolRoot/<locator>
	locator := filepath.Join(".stardust", "tool_results", "sess-1", "fetch_url-abc1234567.md")
	full := filepath.Join(workdir, locator)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const wholeText = "这是被截断的工具结果的全文。"
	if err := os.WriteFile(full, []byte(wholeText), 0o644); err != nil {
		t.Fatalf("write spill: %v", err)
	}

	// 会话的 WorkingDir 就是 toolRoot——这正是被测的那条同源关系。
	sessions := &fileTestSessionStore{
		session: domain.AgentSession{ID: "sess-1", WorkingDir: workdir},
		found:   true,
	}
	srv := NewHTTPServer(Config{AdminToken: "token", Sessions: sessions})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/files?session_id=sess-1&path="+url.QueryEscape(filepath.ToSlash(locator)), nil)
	req.Header.Set("Authorization", "Bearer token")
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，要 200：%s\n"+
			"这说明 spill 的根与 /v1/files 的根不同源——spec §7 说过「根不同源就要补」",
			rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != wholeText {
		t.Errorf("取回的全文 = %q，要 %q", got, wholeText)
	}
}
