# 测试与门禁

本文记录本仓库实际可用的测试命令与几个会浪费时间的环境陷阱。命令均在 2026-07-20 实测通过。

## 一、标准门禁

完成任何改动后，这四条全绿才算完成（见仓库根 `CLAUDE.md`）：

```bash
go build ./...
go vet ./...
go test ./...
gofmt -l .
```

### 陷阱 1：worktree 里必须 `GOWORK=off`

`legion/go.work` 的 use 列表**不包含** `.claude/worktrees/*` 下的工作树。在 worktree 里直接跑会报：

```
pattern ./...: directory prefix . does not contain modules listed in go.work
```

所以在 worktree 中一律加前缀：

```bash
GOWORK=off go test ./...
```

主工作区（`legion/legionAgent`）在 go.work 内，**不需要**加。

### 陷阱 2：`gofmt -l .` 在 Windows 检出上会全量报红

仓库曾无 `.gitattributes`，Windows 检出为 CRLF 而 gofmt 要求 LF，导致它列出全部约 250 个 `.go` 文件。该问题已由 `.gitattributes`（`* text=auto` + `*.go text eol=lf`）修复。

若仍全量报红，说明本地检出早于该提交，刷新一次行尾即可（**执行前确认 `git status` 干净**，此命令会丢弃未提交改动）：

```bash
git rm --cached -r . -q && git reset --hard
```

## 二、race detector

### 什么时候需要

**本项目没有任何模块需要 C 编译器。** `CGO_ENABLED=0 go build ./...` 可以完整构建 —— SQLite 用的是 `modernc.org/sqlite`（纯 Go 移植，非 C 绑定）。

需要 C 编译器的只有 `-race` 本身：Go 的 race detector 基于 ThreadSanitizer，强制 `CGO_ENABLED=1`。

| 操作 | 需要 C 编译器 |
|---|---|
| `go build` / `go test` / 运行服务 | ❌ |
| `go test -race` | ✅ |

改动涉及 goroutine、共享状态、锁、atomic 时应当跑 race。

### 方式 A：Windows（LLVM-MinGW）

已安装于：

```
C:\Users\Administrator\AppData\Local\Microsoft\WinGet\Packages\
  MartinStorsjo.LLVM-MinGW.MSVCRT_Microsoft.Winget.Source_8wekyb3d8bbwe\
  llvm-mingw-20260616-msvcrt-x86_64\bin
```

该目录已在系统 PATH 中，**新开的终端直接可用**：

```bash
GOWORK=off go test -race ./...
```

> 若某个终端报 `-race requires cgo`，通常是该终端进程启动早于 gcc 安装、继承了旧 PATH。重开终端即可；无法重开时显式前置该目录。

### 方式 B：WSL（Ubuntu 22.04）

Go 装在 `/opt/go`，gcc 在 `/usr/bin/gcc`。

```bash
cd /mnt/f/source/stardust/Legion/legion/legionAgent/.claude/worktrees/<worktree名>
export PATH=/opt/go/bin:$PATH GOWORK=off CGO_ENABLED=1 GOOS=linux GOARCH=amd64
go test -race ./...
```

**`GOOS=linux` 不能省。** WSL 的 `/home/<user>/.config/go/env` 里 `GOOS='windows'`，不覆盖的话会交叉编译到 Windows，再拿 Linux gcc 去编译 Windows 的 cgo，报出这个很难联想到根因的错误：

```
gcc: error: unrecognized command-line option '-mthreads'
```

`-mthreads` 是 MinGW 选项，Linux gcc 不认识。

> WSL 里 `git` 认不出 Windows 侧创建的 worktree（其 `.git` 文件记录的是 `F:/...` 形式的路径），但 `go test` 不依赖 git，不受影响。分支切换在 Windows 侧做即可。

### 两种方式不是二选一

Windows 更方便（无需跨文件系统），但**有些测试只有在 Linux 下才真正执行**：

| 测试 | Windows | Linux |
|---|---|---|
| `TestReadEventsFailsLoudOnUnreadableDirectory` | `t.Skip`（chmod 不可复现） | ✅ 真实执行 |
| `internal/port` 的 symlink 系列 | 需开发者模式或管理员权限 | ✅ 原生执行 |

也就是说，涉及**文件权限**或**符号链接**的改动，应当以 WSL 的结果为准。

## 三、变异测试（本仓库的惯例）

新增测试后，把被测的生产代码改回旧写法，确认测试**确实失败**，再改回来。

这条不是形式主义。本轮开发中它抓到过两个假通过的测试：

1. `TestInteractiveModelApprovalPromptLocksMouseScroll` —— 断言恒真，审批 gate 删掉照样通过；
2. `TestSearchContentSkipsOversizedFileAndSaysSo` —— 断言只检查文件名是否出现在输出里，而该文件本身含关键词，去掉大小上限后它会作为正常匹配出现，断言照样成立。

两者都是"测试写完就绿"，只有变异才暴露。

## 四、前端（legionAgentGUI）

```bash
cd frontend
npx vitest run
npm run build      # tsc && vite build
```
