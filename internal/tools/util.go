package tools

import (
	"os"
	"path/filepath"
	"sync"

	"scode/internal/agent"
)

// Resolve absolutizes a tool path against the session working directory.
func Resolve(tc agent.ToolContext, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	base := tc.CWD
	if base == "" {
		if wd, err := os.Getwd(); err == nil {
			base = wd
		}
	}
	return filepath.Join(base, path)
}

// mutationLocks serializes file writes per absolute path so concurrent
// tool calls in one turn cannot interleave writes to the same file
// (pi's file-mutation-queue).
var mutationLocks sync.Map // map[string]*sync.Mutex

func WithFileMutation(path string, fn func() error) error {
	mAny, _ := mutationLocks.LoadOrStore(path, &sync.Mutex{})
	m := mAny.(*sync.Mutex)
	m.Lock()
	defer m.Unlock()
	return fn()
}
