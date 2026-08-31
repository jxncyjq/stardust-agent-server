# 两条分叉的取证与结论：WebRTC 帧升级 / set-of-marks

**日期**：2026-08-31
**结论先说**：两条**都不建议现在做**，但理由完全不同——WebRTC 是「按现在的部署形态，它买不到东西」；set-of-marks 是「它依赖的通道还不存在，先做那条通道才谈得上它」。

---

## 一、WebRTC 帧升级

### 现状通道

CDP screencast（JPEG，quality 60，EveryNthFrame 2）→ Go 按 fps 节流丢帧 → base64 → JSON → SSE → Go 的 sse_bridge → Wails 事件 → 前端拼成 `data:` URI → `<img>`/canvas。

### 实测（`go test -tags chromium -run TestMeasureTheScreencastChannel -v`）

| 页面 | 帧率 | 单帧中位 | base64 后带宽 | 一小时 |
|---|---|---|---|---|
| 稀疏（一个色块） | 7.2 fps | 6.6 KiB | 63 KiB/s | 221 MiB |
| 稠密（400 个渐变块 + 文字，整屏在变） | 6.2 fps | **263 KiB** | **2.1 MiB/s** | **7.5 GiB** |

40 倍的跨度。稠密那一档是逐帧 JPEG 的固有代价：帧间没有任何复用，页面每变一次就重发整幅。

### 为什么仍然不做

1. **出货路径上的消费者是本机的 GUI**，serve 由 GUI 在进程内起、绑 `127.0.0.1:0` 且加固。2.1 MiB/s 在回环上不是带宽问题。WebRTC 真正买到的东西——帧间压缩省带宽、自适应码率、NAT 穿透——**在回环上一样都用不上**。
2. **代价落在刚刚才弄绿的地方**。CDP 给的是 JPEG 图，不是编码后的视频轨；要喂给 WebRTC 得先解 JPEG 再编 VP8/H264。Go 里没有可用的纯 Go 编码器，实际means cgo（libvpx/x264）——而三平台打包是**这一周才第一次跑通**的（见 GUI package 工作流），把一个 cgo 编解码依赖压上去，风险与收益不成比例。
3. **没有症状**。没有掉帧、没有界面卡顿的报告；帧率 7 fps 是我们自己设的节流值，不是传输能力的上限。

### 什么时候该重新考虑（写下来，免得下次凭感觉决定）

- **远程接管成为产品目标**：serve 暴露给非本机的观看者。那时 2.1 MiB/s 是真的带宽，且需要自适应码率。
- **帧率要求显著高于 8 fps**（比如要看动画、要跟拖拽）。逐帧 JPEG 在 20~30 fps 上会把 CPU 吃掉，而那正是帧间压缩的主场。

### 在此之前，如果稠密页面真的开始咬人

按代价从小到大：①按帧大小自适应降 quality（screencaster 内部，一处改动）；②提高 `EveryNthFrame`；③换成本机 HTTP 上的 MJPEG 二进制流，绕掉 base64 与 JSON（+33% 与两次字符串搬运）——但注意 `<img src>` 带不了 Bearer 头，而把令牌放进 URL 是不能接受的，所以这条要先解决鉴权形态。

**不要**先跳到 WebRTC：上面三条的总代价都比它小一个量级。

---

## 二、set-of-marks

### 它依赖的东西不存在

set-of-marks 的全部意义是**让看得见图的模型直接指着元素说「点 7 号」**。所以它要求一条链：截图 → 标注 → **作为工具结果送进模型的上下文**。

这条链在第三段断了：

```go
type ToolResult struct {
    CallID  string
    Success bool
    Output  string   // ← 只有文本
    Error   string
}
```

`port.Message` 有 `Images []string`（data URI），但注释写明**只在 user 消息上**（`internal/port/ports.go`）。也就是说：今天做出来的标注图，没有任何一条路径能把它交给模型。做了就是一个没人消费的产物——这个仓这个月已经栽过两次同形的（内置 Chromium 那一级、macOS 那份调通却没有调用方的沙箱 profile）。

第二个缺口小一些：`browser.Element` 只有 `ref/role/name/value`，**没有几何**。标注需要每个 ref 的包围盒（CDP `DOM.getBoxModel`，或 go-rod 的 `element.Shape()`）。

### 最小可用路径（按依赖顺序，不可跳）

1. **工具结果的图像通道**：`ToolResult` 加图像字段，runtime 的工具循环把它作为紧随其后的 user 消息附上（OpenAI 兼容端点普遍不接受 tool 消息里带图）。这是跨 domain/runtime/port 三层的改动，且要确认在用的模型端点确实支持视觉输入——**这一步与浏览器无关，是多模态管线本身**。
2. **元素几何**：`Element` 加 `rect`，来源与 ref 同一次遍历（避免第二次 DOM 往返与 ref 漂移）。
3. **标注与回指**：在截图上画编号框，返回 `mark → ref` 映射；模型说「点 7」，我们转成既有的 `browser_click{ref}`——**ref 语义不变**，这是它能安全接进来的前提。

### 建议

第 1 步是真正的决策点，而它**不是浏览器的事**：一旦工具结果能带图，`browser_read` 才有必要产出标注图；在那之前，set-of-marks 只能做成一个自己看的调试玩具。

---

## 三、这次留下的东西

`internal/browser/screencast_measure_chromium_test.go`——度量探针，不是回归测试（它永远绿，结论在日志里）。下次谁要重开这个话题，先跑它，拿新的数字说话，而不是拿这份文档里的数字（页面、机器、Chromium 版本都会变）。
