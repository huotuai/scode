//go:build windows

package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"scode/internal/llm"
)

// Windows clipboard image reader. Priority: a raw PNG payload (what
// browsers and some screenshot tools offer), then the bitmap formats a
// screenshot leaves behind (CF_DIBV5 / CF_DIB, re-encoded to PNG), then
// an Explorer-copied image FILE (CF_HDROP — "copy file, paste into the
// TUI").

const (
	cfDIB    = 8
	cfHDROP  = 15
	cfDIBV5  = 17
	maxFiles = 16

	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable") // used by formatAvailable (the on-demand read path)
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procRegisterClipboardFormatW   = user32.NewProc("RegisterClipboardFormatW")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procDragQueryFileW             = shell32.NewProc("DragQueryFileW")
	procEnumClipboardFormats       = user32.NewProc("EnumClipboardFormats")
	procGetClipboardFormatNameW    = user32.NewProc("GetClipboardFormatNameW")

	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
)

// pngFormatIDs are the registered clipboard format IDs for raw PNG
// payloads ("PNG" is what Chrome uses; "image/png" appears too).
var pngFormatIDs = sync.OnceValue(func() []uintptr {
	var ids []uintptr
	for _, name := range []string{"PNG", "image/png"} {
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			continue
		}
		if id, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(p))); id != 0 {
			ids = append(ids, id)
		}
	}
	return ids
})

func readClipboardImage() ([]byte, error) {
	r, _, _ := procOpenClipboard.Call(0)
	if r == 0 {
		return nil, errNoClipboardImage // busy or empty: caller pastes text
	}
	defer procCloseClipboard.Call() //nolint:errcheck

	// 1. Raw PNG payload.
	for _, id := range pngFormatIDs() {
		if !formatAvailable(id) {
			continue
		}
		if data, ok := clipboardBytes(id); ok && llm.DetectImageMIME(data) == "image/png" {
			return data, nil
		}
	}
	// 2. Bitmap formats (screenshots): DIB → PNG.
	for _, format := range []uintptr{cfDIBV5, cfDIB} {
		if !formatAvailable(format) {
			continue
		}
		if dib, ok := clipboardBytes(format); ok {
			if png, err := dibToPNG(dib); err == nil {
				return png, nil
			}
		}
	}
	// 3. Explorer-copied files: the first file that reads as an image.
	if formatAvailable(cfHDROP) {
		if data, ok := clipboardFileImage(); ok {
			return data, nil
		}
	}
	return nil, errNoClipboardImage
}

func formatAvailable(format uintptr) bool {
	r, _, _ := procIsClipboardFormatAvailable.Call(format)
	return r != 0
}

// writeClipboardText replaces the clipboard's text content — the
// write-side mirror of clipboardBytes: allocate a movable global
// block, copy the NUL-terminated UTF-16 text in, hand ownership to
// the system via SetClipboardData (a failed handoff frees the block).
func writeClipboardText(text string) bool {
	chars := utf16.Encode([]rune(text + "\x00"))
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(len(chars)*2))
	if h == 0 {
		return false
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		procGlobalFree.Call(h) //nolint:errcheck
		return false
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(chars)), chars)
	procGlobalUnlock.Call(h) //nolint:errcheck

	free := func() { procGlobalFree.Call(h) } //nolint:errcheck
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		free()
		return false
	}
	defer procCloseClipboard.Call() //nolint:errcheck
	if r, _, _ := procEmptyClipboard.Call(); r == 0 {
		free()
		return false
	}
	if r, _, _ := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		free()
		return false
	}
	return true // the system owns the block now
}

// clipboardBytes copies a clipboard format's global-memory payload out
// (the handle stays owned by the clipboard).
func clipboardBytes(format uintptr) ([]byte, bool) {
	h, _, _ := procGetClipboardData.Call(format)
	if h == 0 {
		return nil, false
	}
	size, _, _ := procGlobalSize.Call(h)
	if size == 0 {
		return nil, false
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return nil, false
	}
	defer procGlobalUnlock.Call(h) //nolint:errcheck
	data := make([]byte, size)
	copy(data, unsafe.Slice((*byte)(unsafe.Pointer(ptr)), size))
	return data, true
}

// clipboardFileImage reads the file list of an Explorer copy (CF_HDROP)
// and returns the content of the first file that sniffs as an image.
func clipboardFileImage() ([]byte, bool) {
	h, _, _ := procGetClipboardData.Call(cfHDROP)
	if h == 0 {
		return nil, false
	}
	count, _, _ := procDragQueryFileW.Call(h, ^uintptr(0), 0, 0)
	if count == 0 {
		return nil, false
	}
	if count > maxFiles {
		count = maxFiles
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	for i := uintptr(0); i < count; i++ {
		n, _, _ := procDragQueryFileW.Call(h, i, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 {
			continue
		}
		path := windows.UTF16ToString(buf[:n])
		if !imageFileExt(path) {
			continue
		}
		if data, err := os.ReadFile(path); err == nil && llm.DetectImageMIME(data) != "" {
			return data, true
		}
	}
	return nil, false
}

func imageFileExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

// clipboardFormatNames lists the formats the clipboard currently offers
// (manual-test diagnostics).
func clipboardFormatNames() []string {
	r, _, _ := procOpenClipboard.Call(0)
	if r == 0 {
		return nil
	}
	defer procCloseClipboard.Call() //nolint:errcheck
	known := map[uintptr]string{
		1: "CF_TEXT", 2: "CF_BITMAP", 8: "CF_DIB", 13: "CF_UNICODETEXT",
		15: "CF_HDROP", 17: "CF_DIBV5",
	}
	var names []string
	for f := uintptr(0); ; {
		next, _, _ := procEnumClipboardFormats.Call(f)
		if next == 0 {
			break
		}
		if name, ok := known[next]; ok {
			names = append(names, fmt.Sprintf("%s", name))
		} else if n := registeredFormatName(next); n != "" {
			names = append(names, n)
		} else {
			names = append(names, fmt.Sprintf("fmt:%d", next))
		}
		f = next
	}
	return names
}

// registeredFormatName resolves a registered format id to its name.
func registeredFormatName(id uintptr) string {
	var buf [64]uint16
	n, _, _ := procGetClipboardFormatNameW.Call(id, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || n >= uintptr(len(buf)) {
		return ""
	}
	return string(utf16.Decode(buf[:n]))
}
