package domain

import "testing"

func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"", ModeAuto, true},
		{"  ", ModeAuto, true},
		{"auto", ModeAuto, true},
		{"manual", ModeManual, true},
		{"plan", ModePlan, true},
		{" manual ", ModeManual, true},
		{"MANUAL", "", false}, // 大小写敏感：拒绝，不静默转换
		{"bogus", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeMode(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("NormalizeMode(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
