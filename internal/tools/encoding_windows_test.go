//go:build windows

package tools

import "testing"

// The GBK bytes for "中文测试" (the OEM code page on a Chinese Windows)
// must decode to the original text; ASCII and valid UTF-8 pass through.
func TestDecodeCodePageGBK(t *testing.T) {
	gbk := string([]byte{0xd6, 0xd0, 0xce, 0xc4, 0xb2, 0xe2, 0xca, 0xd4})
	got, ok := decodeCodePage(gbk, 936)
	if !ok || got != "中文测试" {
		t.Fatalf("decodeCodePage(GBK) = %q, %v; want 中文测试, true", got, ok)
	}
}

func TestNormalizeShellOutput(t *testing.T) {
	if got := normalizeShellOutput("hello\r\n"); got != "hello\r\n" {
		t.Fatalf("ASCII changed: %q", got)
	}
	utf8s := "中文测试"
	if got := normalizeShellOutput(utf8s); got != utf8s {
		t.Fatalf("valid UTF-8 changed: %q", got)
	}
}
