package agent

import "testing"

func TestSanitizeTitle(t *testing.T) {
	cases := map[string]string{
		"  修复登录 bug\n多余的一行": "修复登录 bug",
		"\"重写 build 脚本\"":       "重写 build 脚本",
		"标题:部署流程":              "部署流程",
		"标题：部署流程":             "部署流程",
		"Title: Fix parser.":     "Fix parser.",
		"`短标题`":                 "短标题",
	}
	for in, want := range cases {
		if got := SanitizeTitle(in); got != want {
			t.Errorf("SanitizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeTitleCapsLength(t *testing.T) {
	long := ""
	for i := 0; i < 40; i++ {
		long += "字"
	}
	if got := SanitizeTitle(long); len([]rune(got)) != 24 {
		t.Fatalf("SanitizeTitle caps at 24 runes, got %d", len([]rune(got)))
	}
}
