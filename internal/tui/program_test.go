package tui

import (
	"bytes"
	"io"
	"sync"
)

// syncWriter serializes renderer writes for test output capture.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// scriptWriter feeds keystrokes with small delays so the input reader
// parses them as distinct key events.
type scriptWriter struct {
	w io.Writer
}

func (s scriptWriter) write(s2 string) error {
	_, err := io.WriteString(s.w, s2)
	return err
}
