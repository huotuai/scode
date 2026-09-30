//go:build windows

package tools

import (
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"
)

var procGetOEMCP = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetOEMCP")

// normalizeShellOutput converts Windows legacy code-page output to UTF-8.
//
// A console-less cmd.exe (the confined shell on Windows) and native Windows
// console programs emit bytes in the system OEM code page — 936/GBK on a
// Chinese system — which are not valid UTF-8. Left as-is, every non-ASCII
// byte folded to U+FFFD by JSON marshaling, so Chinese output reached the
// model and the UI as mojibake. bash/MSYS and modern tools emit UTF-8, which
// passes through untouched, so the UTF-8 check cleanly separates the two.
func normalizeShellOutput(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	cp := uint32(0)
	if r, _, _ := procGetOEMCP.Call(); r != 0 {
		cp = uint32(r)
	}
	if cp == 0 {
		cp = windows.GetACP()
	}
	if out, ok := decodeCodePage(s, cp); ok {
		return out
	}
	return s
}

// decodeCodePage converts s from the given Windows code page to UTF-8 via
// MultiByteToWideChar (ASCII passes through unchanged). ok is false when the
// conversion is unavailable, so the caller keeps the original bytes.
func decodeCodePage(s string, cp uint32) (string, bool) {
	if cp == 0 || cp == 65001 {
		return "", false
	}
	b := []byte(s)
	if len(b) == 0 {
		return "", false
	}
	n, err := windows.MultiByteToWideChar(cp, 0, &b[0], int32(len(b)), nil, 0)
	if err != nil || n <= 0 {
		return "", false
	}
	buf := make([]uint16, n)
	n2, err := windows.MultiByteToWideChar(cp, 0, &b[0], int32(len(b)), &buf[0], n)
	if err != nil || n2 <= 0 {
		return "", false
	}
	return string(utf16.Decode(buf[:n2])), true
}
