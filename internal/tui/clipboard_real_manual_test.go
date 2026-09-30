//go:build windows

package tui

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"scode/internal/llm"
)

// Manual verification of the real Win32 clipboard path: put an image on
// the clipboard first (e.g. PowerShell -STA:
//
//	Add-Type -AssemblyName System.Windows.Forms
//	[Windows.Forms.Clipboard]::SetImage([Drawing.Bitmap]::new("x.png"))
//
// or just Win+Shift+S a region), then run with SCODE_CLIP_TEST=1.
func TestReadClipboardImageReal(t *testing.T) {
	if os.Getenv("SCODE_CLIP_TEST") == "" {
		t.Skip("set SCODE_CLIP_TEST=1 with an image on the clipboard")
	}
	data, err := readClipboardImage()
	if err != nil {
		t.Fatalf("readClipboardImage: %v", err)
	}
	if mime := llm.DetectImageMIME(data); mime == "" {
		t.Fatalf("payload is not a supported image (%d bytes)", len(data))
	} else {
		t.Logf("clipboard image: %s, %d bytes", mime, len(data))
	}
}

// Manual verification of the on-demand read's real seams: the format
// probe against the live clipboard (same setup as above).
func TestClipboardFormatsReal(t *testing.T) {
	if os.Getenv("SCODE_CLIP_TEST") == "" {
		t.Skip("set SCODE_CLIP_TEST=1 with an image on the clipboard")
	}
	formats := clipboardFormatNames()
	t.Logf("formats=%v", formats)
	data, err := readClipboardImage()
	if err != nil {
		t.Fatalf("image on the clipboard but the on-demand read failed: %v", err)
	}
	t.Logf("read %d bytes", len(data))
}

// Manual verification of the real Win32 text-write path (right-click
// copy): writes, then reads the bytes back through the read-side
// GlobalLock copy and compares.
func TestWriteClipboardTextReal(t *testing.T) {
	if os.Getenv("SCODE_CLIP_TEST") == "" {
		t.Skip("set SCODE_CLIP_TEST=1 to verify the real clipboard write")
	}
	const want = "scode 剪贴板写入测试 ✨"
	if !writeClipboardText(want) {
		t.Fatal("writeClipboardText failed")
	}
	if !formatAvailable(cfUnicodeText) {
		t.Fatal("CF_UNICODETEXT missing after write")
	}
	// GetClipboardData needs an open clipboard (the read paths open
	// first; the manual readback here must too).
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		t.Fatal("cannot open clipboard for readback")
	}
	defer procCloseClipboard.Call() //nolint:errcheck
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		t.Fatal("CF_UNICODETEXT missing after write")
	}
	size, _, _ := procGlobalSize.Call(h)
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 || size < 4 {
		t.Fatalf("lock=%d size=%d", ptr, size)
	}
	raw := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), size/2)
	procGlobalUnlock.Call(h) //nolint:errcheck
	if got := windows.UTF16ToString(raw); got != want {
		t.Fatalf("read back %q, want %q", got, want)
	}
}
