package cognitive

import "testing"

func TestCJKAwareCounter(t *testing.T) {
	c := NewCJKTokenCounter()
	cases := []struct {
		name string
		in   string
		min  int // 下界断言，避免脆弱精确值
	}{
		{"ascii_words", "the quick brown fox", 3},
		{"chinese_no_space", "服务端是唯一真相源结算消耗判定", 8}, // 旧算法会得 1
		{"mixed", "调用 list_tools 获取工具 schema", 6},
		{"empty", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.Count(tc.in)
			if got < tc.min {
				t.Fatalf("Count(%q) = %d, want >= %d", tc.in, got, tc.min)
			}
		})
	}
}

func TestCJKCounterBeatsWhitespaceOnCJK(t *testing.T) {
	c := NewCJKTokenCounter()
	cjk := "这是一段没有空格的中文文本用于压缩阈值判断"
	if c.Count(cjk) < 10 {
		t.Fatalf("CJK text under-counted: got %d", c.Count(cjk))
	}
}
