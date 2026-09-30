//go:build !windows

package sandbox

import "path/filepath"

// evalExistingPath resolves one EXISTING path to its canonical form. On the
// unix backends EvalSymlinks is the faithful primitive (it walks every
// component), so the ancestor walk in Canonical only needs it for the
// deepest existing prefix.
func evalExistingPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
