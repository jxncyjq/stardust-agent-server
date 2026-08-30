package browser

import (
	"errors"
	"fmt"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// inputTarget 是注入的落点：一批事件最终要打到的那张页。
//
// 单独抽出来，是因为这里真正要保证的东西发生在**失败之后**——批次半途出错时留下的
// 那点状态。真机上很难在指定的第 i 条事件上稳定失败，而那正是需要钉住的一条路径。
type inputTarget interface {
	MoveMouse(p proto.Point) error
	MouseDown(button proto.InputMouseButton) error
	MouseUp(button proto.InputMouseButton) error
	Click(button proto.InputMouseButton) error
	Scroll(deltaX, deltaY float64) error
	KeyDown(k input.Key) error
	KeyUp(k input.Key) error
	InsertText(text string) error
}

// pageTarget 把 go-rod 的页接到 inputTarget 上。点击次数固定为 1：接管转发的是浏览器
// 里逐条发生的原始事件，双击由 GUI 连发两条，不在这里合成。
type pageTarget struct{ page *rod.Page }

func (t pageTarget) MoveMouse(p proto.Point) error { return t.page.Mouse.MoveTo(p) }
func (t pageTarget) MouseDown(button proto.InputMouseButton) error {
	return t.page.Mouse.Down(button, 1)
}
func (t pageTarget) MouseUp(button proto.InputMouseButton) error {
	return t.page.Mouse.Up(button, 1)
}
func (t pageTarget) Click(button proto.InputMouseButton) error {
	return t.page.Mouse.Click(button, 1)
}
func (t pageTarget) Scroll(deltaX, deltaY float64) error {
	return t.page.Mouse.Scroll(deltaX, deltaY, 1)
}
func (t pageTarget) KeyDown(k input.Key) error    { return t.page.Keyboard.Press(k) }
func (t pageTarget) KeyUp(k input.Key) error      { return t.page.Keyboard.Release(k) }
func (t pageTarget) InsertText(text string) error { return t.page.InsertText(text) }

// injectBatch 按顺序注入一批事件，并在中途失败时收拾自己按下的键。
//
// **这里没有回滚**，也给不出回滚：已经发生的点击、已经输入的字符，浏览器里没有撤销
// 这回事。承诺只有两条：
//
//  1. 不把页面留在半按状态。批次里按下过、还没轮到抬起的鼠标键与普通键，失败时放掉。
//     留一个按住的鼠标键，用户看到的是「浏览器坏了」——之后每一次移动都成了拖拽，
//     每一次点击都落在选区上。
//  2. 错误里带上做到了第几条。调用方的下一步取决于这个数：重发整批会把已经生效的前
//     几条再做一遍（又点一次「提交」），而它无从判断。
//
// 只在**失败**时收尾。成功的批次末尾按键还按着是正常的：GUI 把 mousedown 与 mouseup
// 发在不同批次里，拖拽就是这么来的，替它抬起会把每一次拖拽都掐断在起点。
//
// 收尾自己也可能失败（页真的没了）。那些错误 join 进返回值一起上报，不吞——正是它们
// 说明「按住的键没能放掉」，用户此刻多半得重开会话。
func injectBatch(target inputTarget, events []InputEvent, vw, vh float64) error {
	var (
		buttonsDown []proto.InputMouseButton
		keysDown    []input.Key
	)
	for i, ev := range events {
		if err := injectOne(target, ev, vw, vh); err != nil {
			failure := &PartialInjection{
				Applied: i,
				Total:   len(events),
				Failed:  ev.Type,
				Err:     err,
			}
			return errors.Join(failure, releaseHeld(target, buttonsDown, keysDown))
		}
		// 记账放在成功之后：没打出去的按下不需要抬起。
		switch ev.Type {
		case "mousedown":
			buttonsDown = append(buttonsDown, mouseButton(ev.Button))
		case "mouseup":
			buttonsDown = removeButton(buttonsDown, mouseButton(ev.Button))
		case "keydown":
			// 名字在这里必定合法：validateInputEvents 在整批之前跑过，injectOne 也刚
			// 用它成功按下过。解析不出来属编程错误，不猜一个键。
			k, err := keyToInputKey(ev.Key)
			if err != nil {
				return errors.Join(fmt.Errorf("track pressed key %q: %w", ev.Key, err),
					releaseHeld(target, buttonsDown, keysDown))
			}
			keysDown = append(keysDown, k)
		case "keyup":
			k, err := keyToInputKey(ev.Key)
			if err != nil {
				return errors.Join(fmt.Errorf("track released key %q: %w", ev.Key, err),
					releaseHeld(target, buttonsDown, keysDown))
			}
			keysDown = removeKey(keysDown, k)
		}
	}
	return nil
}

// PartialInjection 说明一批注入停在了哪里：Applied 条已经发生，其余没发出去。
//
// 单独一个类型而不是把数字写进句子里，是因为调用方要**据此决定下一步**：重发整批会
// 把已经生效的前 Applied 条再做一遍（又点一次「提交」）。让它去解析一句话，等于把这
// 个判断建在措辞上。
type PartialInjection struct {
	Applied int    // 失败之前成功注入的事件数
	Total   int    // 这一批的总数
	Failed  string // 出错那条的事件类型
	Err     error
}

func (e *PartialInjection) Error() string {
	return fmt.Sprintf("inject event %d (%s) after applying %d of %d: %v",
		e.Applied, e.Failed, e.Applied, e.Total, e.Err)
}

func (e *PartialInjection) Unwrap() error { return e.Err }

// releaseHeld 反序放掉还按着的键，**每一个都试**：其中一个失败不该让其余的留在按下
// 状态，那正是「键盘坏了」的来源。
func releaseHeld(target inputTarget, buttons []proto.InputMouseButton, keys []input.Key) error {
	var errs []error
	for i := len(keys) - 1; i >= 0; i-- {
		if err := target.KeyUp(keys[i]); err != nil {
			errs = append(errs, fmt.Errorf("release key held by the failed batch: %w", err))
		}
	}
	for i := len(buttons) - 1; i >= 0; i-- {
		if err := target.MouseUp(buttons[i]); err != nil {
			errs = append(errs, fmt.Errorf("release mouse button %q held by the failed batch: %w",
				buttons[i], err))
		}
	}
	return errors.Join(errs...)
}

// removeButton 去掉最后一次按下的那一个（同一个键重复按下只对应一次抬起）。
func removeButton(list []proto.InputMouseButton, want proto.InputMouseButton) []proto.InputMouseButton {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == want {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

func removeKey(list []input.Key, want input.Key) []input.Key {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == want {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}
