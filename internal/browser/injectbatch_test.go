package browser

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// 一批注入中途失败时，前面几条**已经发生了**——浏览器里的点击与输入没有撤销这回事，
// 所以「回滚」不是这里能给的承诺。
//
// 能给、也必须给的是两件：
//
//  1. **不把页面留在半按状态**。批次里按下过而没来得及抬起的鼠标键/普通键，失败时
//     要放掉。留一个按住的鼠标键，用户看到的是「浏览器坏了」：之后每一次移动都成了
//     拖拽，每一次点击都落在选区上。
//  2. **说清楚做到第几条**。调用方的下一步取决于这个数——重发整批会把已经生效的前
//     几条再做一遍（又点一次「提交」），而它无从判断。
//
// 这些用假的注入目标测：真机上很难在指定的第 i 条上稳定失败，而这里要钉的正是失败
// 之后的那段收尾。

// fakeInjector 记录收到的动作，并可在第 failAt 次动作上失败。
type fakeInjector struct {
	actions []string
	failAt  int // 第几次动作失败（1 起算）；0 表示不失败
	calls   int
}

var errInjectorBroke = errors.New("the page went away")

func (f *fakeInjector) record(action string) error {
	f.calls++
	if f.failAt > 0 && f.calls == f.failAt {
		return errInjectorBroke
	}
	f.actions = append(f.actions, action)
	return nil
}

func (f *fakeInjector) MoveMouse(proto.Point) error { return f.record("move") }
func (f *fakeInjector) MouseDown(button proto.InputMouseButton) error {
	return f.record("down:" + string(button))
}
func (f *fakeInjector) MouseUp(button proto.InputMouseButton) error {
	return f.record("up:" + string(button))
}
func (f *fakeInjector) Click(button proto.InputMouseButton) error {
	return f.record("click:" + string(button))
}
func (f *fakeInjector) Scroll(float64, float64) error { return f.record("scroll") }
func (f *fakeInjector) KeyDown(k input.Key) error     { return f.record("keydown:" + string(rune(k))) }
func (f *fakeInjector) KeyUp(k input.Key) error       { return f.record("keyup:" + string(rune(k))) }
func (f *fakeInjector) InsertText(text string) error  { return f.record("text:" + text) }
func (f *fakeInjector) did(action string) bool        { return contains(f.actions, action) }
func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestAFailedBatchReleasesWhatItPressed(t *testing.T) {
	t.Parallel()

	// 按下左键、移动、然后第三条动作失败——抬起那一条永远轮不到。
	injector := &fakeInjector{failAt: 4} // down(1) 前有一次 move，所以第 4 次动作是移动之后那条
	events := []InputEvent{
		{Type: "mousedown", X: 0.5, Y: 0.5, Button: "left"},
		{Type: "mousemove", X: 0.6, Y: 0.5},
		{Type: "mousemove", X: 0.7, Y: 0.5},
	}

	err := injectBatch(injector, events, 100, 100)
	if err == nil {
		t.Fatal("a batch whose injector failed reported success")
	}
	if !injector.did("up:left") {
		t.Errorf("the left button was left pressed: %v\n"+
			"every later move becomes a drag and every later click lands on a selection", injector.actions)
	}
}

func TestAFailedBatchReleasesKeysItHeld(t *testing.T) {
	t.Parallel()

	injector := &fakeInjector{failAt: 2}
	events := []InputEvent{
		{Type: "keydown", Key: "a"},
		{Type: "keydown", Key: "b"},
	}

	if err := injectBatch(injector, events, 100, 100); err == nil {
		t.Fatal("a batch whose injector failed reported success")
	}
	if !injector.did("keyup:a") {
		t.Errorf("the key stayed down: %v", injector.actions)
	}
}

// TestASuccessfulBatchLeavesThePressesAlone 是上面那条的边界，也是它不能过头的地方：
// GUI 把 mousedown 与 mouseup 发在**不同批次**里（拖拽就是这么来的）。成功的批次末尾
// 按键还按着是正常的，替它抬起会把每一次拖拽都掐断在起点。
func TestASuccessfulBatchLeavesThePressesAlone(t *testing.T) {
	t.Parallel()

	injector := &fakeInjector{}
	events := []InputEvent{{Type: "mousedown", X: 0.5, Y: 0.5, Button: "left"}}

	if err := injectBatch(injector, events, 100, 100); err != nil {
		t.Fatalf("injectBatch: %v", err)
	}
	if injector.did("up:left") {
		t.Errorf("a successful batch released the button by itself: %v\n"+
			"that breaks drags, which are exactly mousedown and mouseup in separate batches", injector.actions)
	}
}

// TestAMatchedReleaseIsNotDoneTwice：批次里自己抬起过的键，收尾时不该再抬一次。
func TestAMatchedReleaseIsNotDoneTwice(t *testing.T) {
	t.Parallel()

	injector := &fakeInjector{failAt: 5} // down, up, 然后第三条失败
	events := []InputEvent{
		{Type: "mousedown", X: 0.5, Y: 0.5, Button: "left"},
		{Type: "mouseup", X: 0.5, Y: 0.5, Button: "left"},
		{Type: "mousemove", X: 0.6, Y: 0.6},
	}

	if err := injectBatch(injector, events, 100, 100); err == nil {
		t.Fatal("expected the batch to fail")
	}
	ups := 0
	for _, action := range injector.actions {
		if action == "up:left" {
			ups++
		}
	}
	if ups != 1 {
		t.Errorf("left button released %d times, want exactly 1: %v", ups, injector.actions)
	}
}

// TestTheErrorSaysHowFarItGot：调用方的下一步取决于这个数。重发整批会把已经生效的
// 前几条再做一遍（又点一次「提交」），而它无从判断。
func TestTheErrorSaysHowFarItGot(t *testing.T) {
	t.Parallel()

	injector := &fakeInjector{failAt: 3}
	events := []InputEvent{
		{Type: "char", Text: "a"},
		{Type: "char", Text: "b"},
		{Type: "char", Text: "c"},
	}

	err := injectBatch(injector, events, 100, 100)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "2 of 3") {
		t.Errorf("error = %v, want it to say how many events were applied before the failure", err)
	}
}
