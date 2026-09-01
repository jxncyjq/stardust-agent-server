package cli

import (
	"testing"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// 这一组守 model_profile 这条线（final-review.md I-3）。
//
// 它原先在三个调用点上是字面 ""，而 Runtime 类型里根本没有「模型档位」这个信息——
// 不是忘了传，是这个接缝上拿不到。后果是 spec §4.1 把 model_profile 列进
// assistant/message 载荷，P3 的轨迹里这一栏**永远空白**，而且没有任何东西会报错。
//
// 断言的是**装配的结果**（这个字段真的带着一个非空的档位名到达运行时配置），不是
// 「代码里有那一行」——照 TestEveryBrowserConfigKeyReachesTheRuntime 的范式。

func TestTheModelProfileReachesTheDefaultRunnerRuntimeConfig(t *testing.T) {
	t.Parallel()

	cfg := buildDefaultRunnerConfig(
		nil, nil, nil, nil,
		config.RuntimeConfig{},
		nil, nil, nil, nil,
		nil,
		nil,
		taskgate.NewTaskGate(),
		nil,
		"deep",
	)

	if cfg.ModelProfile != "deep" {
		t.Fatalf("buildDefaultRunnerConfig().ModelProfile = %q, want %q："+
			"默认 agent 的任务（GUI 的主路径）在轨迹里看不出用的是哪个模型", cfg.ModelProfile, "deep")
	}
}

// TestTheResolvedModelProfileNeverComesBackEmpty 守取值规则本身：空串在轨迹里与
// 「装配漏传了这个字段」长得一模一样，所以任何一种配置形态都必须解出一个名字。
func TestTheResolvedModelProfileNeverComesBackEmpty(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     config.MaasConfig
		profile string
		baseURL string
		want    string
	}{
		{
			name:    "显式档位优先",
			cfg:     config.MaasConfig{DefaultProfile: "default"},
			profile: "deep",
			want:    "deep",
		},
		{
			name: "没有显式档位就用 default_profile",
			cfg:  config.MaasConfig{DefaultProfile: "default"},
			want: "default",
		},
		{
			name: "只有裸 base_url 的部署没有档位这个概念",
			cfg:  config.MaasConfig{BaseURL: "http://maas.local"},
			want: "maas",
		},
		{
			name: "什么都没配就是离线录制客户端",
			cfg:  config.MaasConfig{},
			want: "recording",
		},
		{
			name:    "--maas-url 建的是绕过所有档位的临时客户端",
			cfg:     config.MaasConfig{DefaultProfile: "default"},
			baseURL: "http://localhost:9999",
			want:    "custom-maas",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runModelProfile(tc.cfg, tc.profile, tc.baseURL); got != tc.want {
				t.Errorf("runModelProfile() = %q, want %q", got, tc.want)
			}
		})
	}
}
