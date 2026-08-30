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
	// Modifiers 是**这一条事件**按住的修饰键（ctrl|shift|alt|meta），随事件走而不是
	// 靠前后两条 keydown/keyup 维持状态。
	//
	// 选按事件携带而非有状态按下，是因为注入是一串**互不相干的 HTTP 请求**：丢一条
	// keyup（页面切走、面板关掉、请求失败）就会把浏览器永久留在 Ctrl 按住的状态里，
	// 而那个状态在下一位使用者眼里是「键盘坏了」。按事件携带则每条事件自带完整语义，
	// 注入前按下、注入后（含出错路径）释放，跨请求不留残留。
	Modifiers []string `json:"modifiers,omitempty"`
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

// modifierKeys 把契约里的修饰键名映射到 go-rod 的键常量。名字是小写的四个短名，
// 与 JS 的 ctrlKey/shiftKey/altKey/metaKey 一一对应，前端不必再翻译一次。
//
// 一律取 Left 侧：CDP 的修饰键位掩码不区分左右，左右之分只影响 code 字段，而没有
// 任何快捷键是按 ControlLeft 与 ControlRight 分派的。
var modifierKeys = map[string]input.Key{
	"ctrl":  input.ControlLeft,
	"shift": input.ShiftLeft,
	"alt":   input.AltLeft,
	"meta":  input.MetaLeft,
}

// modifierNamesByKeyName 认得那些**被当成键发过来**的修饰键。它们曾经只落进
// keyToInputKey 的「unsupported key」分支，于是 GUI 每按一次 Shift 都发出一条必定
// 400 的请求，而作者读到的信息是「这个键不支持」——真正的答案是「它不是键，它是
// 另一个字段」。
var modifierNamesByKeyName = map[string]string{
	"Control": "ctrl",
	"Shift":   "shift",
	"Alt":     "alt",
	"Meta":    "meta",
}

// modifierToInputKey 解析一个修饰键名，未知的一律报错——猜一个（把 "cmd" 当
// meta、把 "ctrlKey" 当 ctrl）意味着替调用方注入一个它没要求的快捷键。
func modifierToInputKey(name string) (input.Key, error) {
	k, ok := modifierKeys[name]
	if !ok {
		return 0, fmt.Errorf("unknown modifier %q: use one of ctrl, shift, alt, meta", name)
	}
	return k, nil
}

// validateModifiers 校验一条事件的修饰键集合。
//
// char 事件拒收 shift 以外的修饰键：char 走 InsertText，把文本原样放进页面，没有
// 任何修饰键能改变这一点。收下 "ctrl" 就等于把一次「复制」悄悄变成「输入一个字母
// c」——正是这次要修的那个缺陷。shift 是例外且只是例外：移位后的字符**已经在文本
// 里**，它不额外表达什么。
func validateModifiers(ev InputEvent) error {
	for _, name := range ev.Modifiers {
		if _, err := modifierToInputKey(name); err != nil {
			return err
		}
		if ev.Type == "char" && name != "shift" {
			return fmt.Errorf("char events carry text verbatim and cannot be modified by %q; "+
				"send keydown/keyup with modifiers for a shortcut", name)
		}
	}
	return nil
}

// validateInputEvents 硬校验一整批注入事件；任一条越界即整批拒（fail-loud，不静默跳过单条）。
func validateInputEvents(events []InputEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("input batch is empty")
	}
	if len(events) > maxInputBatch {
		return fmt.Errorf("input batch too large: %d > %d", len(events), maxInputBatch)
	}
	for i, ev := range events {
		if err := validateModifiers(ev); err != nil {
			return fmt.Errorf("event %d (%s): %w", i, ev.Type, err)
		}
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
	if modifier, ok := modifierNamesByKeyName[key]; ok {
		return 0, fmt.Errorf("%q is a modifier, not a key: put %q in this event's \"modifiers\" "+
			"alongside the key it modifies", key, modifier)
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
