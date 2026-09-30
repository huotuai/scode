package tui

import (
	"os"
	"testing"

	"scode/internal/i18n"
)

// TestMain pins the display language to zh for the whole package: the
// assertions below were written against the zh catalog strings, and
// the host's auto-detected locale must not flip them.
func TestMain(m *testing.M) {
	i18n.Set(i18n.Zh)
	os.Exit(m.Run())
}
