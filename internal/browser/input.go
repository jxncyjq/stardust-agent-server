package browser

import (
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/go-rod/rod/lib/input"
)

// InputEvent 是前端接管期间注入的一条归一化输入事件（见 takeover spec §3.1）。
// 坐标 X/Y 为相对视口的 0..1 归一化值，后端 × 当前视口 px 后经 go-rod 注入。
type InputEvent struct {
	Type   string  `json:"type"`             // mousemove|mousedown|mouseup|click|wheel|keydown|keyup|char
	X      float64 `json:"x"`                // 0..1；鼠标类必填
	Y      float64 `json:"y"`                // 0..1
	Button string  `json:"button,omitempty"` // left|right|middle
	DeltaX float64 `json:"deltaX,omitempty"` // wheel
	DeltaY float64 `json:"deltaY,omitempty"`
	Key    string  `json:"key,omitempty"`  // keydown/keyup 的键名（JS e.key）
	Text   string  `json:"text,omitempty"` // char：要插入的文本
}

const (
	maxInputBatch = 256  // 单批事件条数上限，防滥用
	maxKeyLen     = 32   // 键名长度上限
	maxTextLen    = 1024 // char 文本长度上限
)

// mouseTypes / buttonNames 是类型白名单。
var mouseTypes = map[string]bool{
	"mousemove": true, "mousedown": true, "mouseup": true, "click": true, "wheel": true,
}
var buttonNames = map[string]bool{"left": true, "right": true, "middle": true}

// validateInputEvents 硬校验一整批注入事件；任一条越界即整批拒（fail-loud，不静默跳过单条）。
func validateInputEvents(events []InputEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("input batch is empty")
	}
	if len(events) > maxInputBatch {
		return fmt.Errorf("input batch too large: %d > %d", len(events), maxInputBatch)
	}
	for i, ev := range events {
		switch {
		case mouseTypes[ev.Type]:
			if err := checkNormalized(ev.X, ev.Y); err != nil {
				return fmt.Errorf("event %d (%s): %w", i, ev.Type, err)
			}
			if ev.Button != "" && !buttonNames[ev.Button] {
				return fmt.Errorf("event %d (%s): bad button %q", i, ev.Type, ev.Button)
			}
			if ev.Type == "wheel" {
				for _, d := range []float64{ev.DeltaX, ev.DeltaY} {
					if math.IsNaN(d) || math.IsInf(d, 0) {
						return fmt.Errorf("event %d (wheel): non-finite delta %v", i, d)
					}
				}
			}
		case ev.Type == "keydown" || ev.Type == "keyup":
			if ev.Key == "" {
				return fmt.Errorf("event %d (%s): missing key", i, ev.Type)
			}
			if len(ev.Key) > maxKeyLen {
				return fmt.Errorf("event %d (%s): key too long", i, ev.Type)
			}
			if _, err := keyToInputKey(ev.Key); err != nil {
				return fmt.Errorf("event %d (%s): %w", i, ev.Type, err)
			}
		case ev.Type == "char":
			if ev.Text == "" {
				return fmt.Errorf("event %d (char): empty text", i)
			}
			if len(ev.Text) > maxTextLen {
				return fmt.Errorf("event %d (char): text too long", i)
			}
		default:
			return fmt.Errorf("event %d: unknown type %q", i, ev.Type)
		}
	}
	return nil
}

// checkNormalized 断言 x,y ∈ [0,1] 且有限。
func checkNormalized(x, y float64) error {
	for _, v := range []float64{x, y} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("coordinate %v out of [0,1]", v)
		}
	}
	return nil
}

// namedKeys 把常用特殊键的 JS e.key 名映射到 go-rod input.Key 常量（白名单）。
// 可打印单字符走 keyToInputKey 的 rune 分支，不进这张表。
var namedKeys = map[string]input.Key{
	"Enter":      input.Enter,
	"Backspace":  input.Backspace,
	"Tab":        input.Tab,
	"Escape":     input.Escape,
	"Delete":     input.Delete,
	"ArrowLeft":  input.ArrowLeft,
	"ArrowRight": input.ArrowRight,
	"ArrowUp":    input.ArrowUp,
	"ArrowDown":  input.ArrowDown,
	"Home":       input.Home,
	"End":        input.End,
	"PageUp":     input.PageUp,
	"PageDown":   input.PageDown,
}

// keyToInputKey 把 JS e.key 转成 go-rod input.Key：命名键查白名单，
// 单个可打印字符按 rune 直取；其它一律报错（fail-loud，不猜键）。
func keyToInputKey(key string) (input.Key, error) {
	if key == "" {
		return 0, fmt.Errorf("empty key")
	}
	if k, ok := namedKeys[key]; ok {
		return k, nil
	}
	if utf8.RuneCountInString(key) == 1 {
		r, _ := utf8.DecodeRuneInString(key)
		return input.Key(r), nil
	}
	return 0, fmt.Errorf("unsupported key %q", key)
}
