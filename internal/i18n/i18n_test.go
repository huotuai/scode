package i18n

import "testing"

func TestParseLang(t *testing.T) {
	cases := []struct {
		in   string
		want Lang
		ok   bool
	}{
		{"zh", Zh, true},
		{"zh_CN.UTF-8", Zh, true},
		{"zh-Hans-CN", Zh, true},
		{"ZH-TW", Zh, true},
		{"en", En, true},
		{"en_US", En, true},
		{"EN-GB", En, true},
		{"", En, false},
		{"ja_JP", En, false},
		{"  zh_cn  ", Zh, true},
	}
	for _, c := range cases {
		got, ok := parseLang(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseLang(%q) = %v,%v; want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestDetectEnvPrecedence(t *testing.T) {
	t.Setenv("SCODE_LANG", "zh")
	t.Setenv("LANG", "en_US.UTF-8")
	if got := detect(); got != Zh {
		t.Errorf("SCODE_LANG should win over LANG, got %v", got)
	}
	t.Setenv("SCODE_LANG", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	if got := detect(); got != En {
		t.Errorf("LANG=en_US should resolve En, got %v", got)
	}
	t.Setenv("LANG", "zh_CN.UTF-8")
	if got := detect(); got != Zh {
		t.Errorf("LANG=zh_CN should resolve Zh, got %v", got)
	}
}

func TestT(t *testing.T) {
	defer Set(En)
	Set(En)
	if got := T("update.checking"); got != "Checking %s for updates..." {
		t.Errorf("en catalog miss: %q", got)
	}
	Set(Zh)
	if got := T("update.checking"); got != "检查更新（%s）..." {
		t.Errorf("zh catalog miss: %q", got)
	}
	if got := T("no.such.key"); got != "no.such.key" {
		t.Errorf("unknown key should echo the key, got %q", got)
	}
	if got := Tf("update.uptodate", "v1", "v2"); got != "scode v1 已是最新（最新 release: v2）" {
		t.Errorf("Tf formatting broken: %q", got)
	}
}
