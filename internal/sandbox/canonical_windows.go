//go:build windows

package sandbox

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// evalExistingPath resolves one EXISTING path to its canonical form,
// following symlinks AND reparse points (junctions).
//
// filepath.EvalSymlinks is NOT faithful here: Go reports a junction as a
// plain directory (IO_REPARSE_TAG_MOUNT_POINT is not surfaced as
// ModeSymlink), so EvalSymlinks leaves it in place and a target beneath a
// junction keeps its lexical spelling — which is exactly how a
// workspace-internal junction made every containment check pass. The
// handle-based final path is the only reliable primitive: it resolves
// symlinks, junctions and directory reparse points uniformly.
func evalExistingPath(path string) (string, error) {
	// Directories open fine on Windows (Go passes FILE_FLAG_BACKUP_SEMANTICS).
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	return stripLongPathPrefix(windows.UTF16ToString(buf[:n])), nil
}

// GetFinalPathNameByHandle returns a device path: "\\?\C:\dir" for a DOS
// path, "\\?\UNC\server\share" for a share (and "\??\" from NT callers).
// The rest of scode spells paths the plain way, so the prefix is stripped
// back — Go re-adds "\\?\" itself when a path exceeds MAX_PATH.
const (
	longUNCPrefix = "\\\\?\\UNC\\"
	longDOSPrefix = "\\\\?\\"
	longNTPrefix  = "\\??\\"
	uncRoot       = "\\\\" // two backslashes: the UNC root UNC paths carry
)

func stripLongPathPrefix(path string) string {
	if rest, ok := cutPrefix(path, longUNCPrefix); ok {
		return uncRoot + rest
	}
	if rest, ok := cutPrefix(path, longDOSPrefix); ok {
		return rest
	}
	if rest, ok := cutPrefix(path, longNTPrefix); ok {
		return rest
	}
	return path
}

// cutPrefix is strings.CutPrefix, spelled out for clarity at the call site.
func cutPrefix(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):], true
	}
	return s, false
}
