package browser

import (
	"context"
	"testing"
)

// fakeRuntime 是一个满足 RuntimeAPI 的空实现，仅用于锁定接口签名。
type fakeRuntime struct{}

func (fakeRuntime) Open(context.Context, OpenReq) (OpenObservation, error) { return OpenObservation{}, nil }
func (fakeRuntime) Read(context.Context, ReadReq) (Observation, error)     { return Observation{}, nil }
func (fakeRuntime) Click(context.Context, ClickReq) (Observation, error)   { return Observation{}, nil }
func (fakeRuntime) Type(context.Context, TypeReq) (Observation, error)     { return Observation{}, nil }
func (fakeRuntime) Close(context.Context, CloseReq) error                  { return nil }

func TestRuntimeAPISatisfied(t *testing.T) {
	var _ RuntimeAPI = fakeRuntime{}
	// 验证请求类型带必要字段
	_ = OpenReq{URL: "https://x", SessionID: ""}
	_ = ReadReq{SessionID: "s1"}
	_ = ClickReq{SessionID: "s1", Ref: "e1"}
	_ = TypeReq{SessionID: "s1", Ref: "e1", Text: "hi", Submit: true}
	_ = CloseReq{SessionID: "s1"}
}
