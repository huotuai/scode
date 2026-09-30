//go:build !windows

package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"scode/internal/llm"
)

// Non-Windows clipboard image reader: best-effort via the platform's
// clipboard CLI. Wayland wl-paste and X11 xclip stream PNG to stdout;
// macOS pngpaste writes a file. Missing tools mean no image support —
// the keystroke falls through to text paste.
func readClipboardImage() ([]byte, error) {
	if runtime.GOOS == "darwin" {
		return readClipboardImageDarwin()
	}
	for _, cmd := range [][]string{
		{"wl-paste", "-t", "image/png"},
		{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"},
	} {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		out, err := exec.Command(cmd[0], cmd[1:]...).Output()
		if err != nil || llm.DetectImageMIME(out) == "" {
			continue
		}
		return out, nil
	}
	return nil, errNoClipboardImage
}

func readClipboardImageDarwin() ([]byte, error) {
	if _, err := exec.LookPath("pngpaste"); err != nil {
		return nil, errNoClipboardImage
	}
	f, err := os.CreateTemp("", "scode-clip-*.png")
	if err != nil {
		return nil, errNoClipboardImage
	}
	path := f.Name()
	f.Close()             //nolint:errcheck
	defer os.Remove(path) //nolint:errcheck
	if err := exec.Command("pngpaste", path).Run(); err != nil {
		return nil, errNoClipboardImage
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil || llm.DetectImageMIME(data) == "" {
		return nil, errNoClipboardImage
	}
	return data, nil
}

func clipboardFormatNames() []string { return nil }

// writeClipboardText replaces the clipboard's text content, best-effort
// through the desktop's clipboard CLI (any one of them serving the
// current session is enough).
func writeClipboardText(text string) bool {
	for _, cmd := range [][]string{
		{"wl-copy"},                          // Wayland
		{"xclip", "-selection", "clipboard"}, // X11
		{"xsel", "--clipboard", "--input"},   // X11
		{"pbcopy"},                           // macOS
	} {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Stdin = strings.NewReader(text)
		if err := c.Run(); err == nil {
			return true
		}
	}
	return false
}
