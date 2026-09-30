//go:build !windows

package tools

// normalizeShellOutput is a no-op outside Windows: Unix shells emit UTF-8.
func normalizeShellOutput(s string) string { return s }
