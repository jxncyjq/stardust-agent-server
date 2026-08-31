//go:build chromium

package browser

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

// 「把 screencast 升级成 WebRTC」是设计文档里标在主路径之外的一条。它值不值得做，
// 取决于现在这条通道到底差在哪——而在此之前没有人量过。
//
// 这条测试不是回归测试，是**度量**：跑一段真实的 screencast，把帧大小、到达间隔与
// base64 的开销打出来。跑法：
//
//	go test -tags chromium ./internal/browser/ -run TestMeasureTheScreencastChannel -v
//
// 它永远绿（除非通道根本跑不起来）：结论在日志里，不在退出码里。用一个会动的页面，
// 因为静止页面不产帧——那会量出一个漂亮但毫无意义的数。
func TestMeasureTheScreencastChannel(t *testing.T) {
	t.Run("sparse", func(t *testing.T) { measureScreencast(t, sparsePage) })
	// 稠密页面拿上界：只量那个蓝方块，等于把结论建在最好情况上。真实页面有文字、
	// 渐变、图片，JPEG 压不动，帧会大一个量级。
	t.Run("dense", func(t *testing.T) { measureScreencast(t, densePage) })
}

const sparsePage = `<!doctype html><html><body style="margin:0">
<div id="box" style="width:200px;height:200px;background:#3b82f6"></div>
<script>
  let i = 0;
  setInterval(() => {
    i = (i + 7) % 360;
    document.getElementById('box').style.background = 'hsl(' + i + ',70%,50%)';
    document.getElementById('box').style.width = (200 + (i % 100)) + 'px';
  }, 33);
</script></body></html>`

const densePage = `<!doctype html><html><body style="margin:0;font:13px/1.4 monospace">
<div id="grid"></div>
<script>
  const g = document.getElementById('grid');
  let html = '';
  for (let i = 0; i < 400; i++) {
    html += '<div style="display:inline-block;width:120px;height:60px;margin:2px;' +
      'background:linear-gradient(' + (i % 360) + 'deg,#' + (i * 7919 % 0xffffff).toString(16).padStart(6, '0') +
      ',#222);color:#fff;overflow:hidden">row ' + i + ' lorem ipsum dolor sit amet consectetur</div>';
  }
  g.innerHTML = html;
  setInterval(() => { g.style.filter = 'hue-rotate(' + (Date.now() / 10 % 360) + 'deg)'; }, 33);
</script></body></html>`

func measureScreencast(t *testing.T, page string) {

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(t.Context(), CloseReq{})

	obs, err := rt.Open(t.Context(), OpenReq{URL: srv.URL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sessionID := obs.SessionID

	// screencast 由订阅驱动：第一个订阅者到达就开，最后一个走就停（不看视图不推帧）。
	events, unsubscribe, err := rt.Subscribe(sessionID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsubscribe()

	type sample struct {
		bytes int
		at    time.Time
	}
	var samples []sample
	deadline := time.After(6 * time.Second)
collect:
	for {
		select {
		case ev := <-events:
			if ev.Type != EventFrame {
				continue
			}
			data, ok := ev.Data.(map[string]any)
			if !ok {
				t.Fatalf("frame payload is %T, want map", ev.Data)
			}
			b64, _ := data["b64"].(string)
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				t.Fatalf("frame is not base64: %v", err)
			}
			samples = append(samples, sample{bytes: len(raw), at: time.Now()})
			if len(samples) >= 60 {
				break collect
			}
		case <-deadline:
			break collect
		}
	}

	if len(samples) < 5 {
		t.Fatalf("只收到 %d 帧，量不出东西来（页面没动？screencast 没起来？）", len(samples))
	}

	var totalBytes int
	sizes := make([]int, 0, len(samples))
	for _, s := range samples {
		totalBytes += s.bytes
		sizes = append(sizes, s.bytes)
	}
	sort.Ints(sizes)
	span := samples[len(samples)-1].at.Sub(samples[0].at)
	fps := float64(len(samples)-1) / span.Seconds()
	avg := totalBytes / len(samples)

	t.Logf("帧数=%d 时长=%s 实测帧率=%.1f fps", len(samples), span.Round(time.Millisecond), fps)
	t.Logf("单帧字节 中位=%s 最小=%s 最大=%s", human(sizes[len(sizes)/2]), human(sizes[0]), human(sizes[len(sizes)-1]))
	t.Logf("原始带宽=%s/s；base64 之后=%s/s（+33%%）",
		human(int(float64(avg)*fps)), human(int(float64(avg)*fps*4/3)))
	t.Logf("一小时接管：原始 %s，base64 之后 %s",
		human(int(float64(avg)*fps*3600)), human(int(float64(avg)*fps*3600*4/3)))
}

func human(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
