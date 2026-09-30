// Package i18n holds the bilingual (en/zh) catalog for user-facing
// display strings. Model-facing text — tool result markers, prompts,
// tool declarations — is never localized: those literal bytes are part
// of agent protocols and land in session history (prefix stability).
//
// Language resolution (once at init): SCODE_LANG > LC_ALL/LC_MESSAGES/
// LANG > OS UI language (Windows) > English.
package i18n

import (
	"fmt"
	"os"
	"strings"
)

// Lang is a display language.
type Lang int

const (
	En Lang = iota
	Zh
)

// current is the process-wide display language, resolved at init.
var current = detect()

// Current returns the active display language.
func Current() Lang { return current }

// Set overrides the display language (settings panels, tests).
func Set(l Lang) { current = l }

// Message is one catalog entry in both languages.
type Message struct{ En, Zh string }

// T returns the localized string for key; unknown keys fall back to the
// key itself (visible during development, never silently empty).
func T(key string) string {
	m, ok := catalog[key]
	if !ok {
		return key
	}
	if current == Zh && m.Zh != "" {
		return m.Zh
	}
	return m.En
}

// Tf is T plus fmt.Sprintf formatting.
func Tf(key string, args ...any) string { return fmt.Sprintf(T(key), args...) }

// detect resolves the display language from the environment, then the
// OS (locale_windows.go), defaulting to English.
func detect() Lang {
	for _, env := range []string{"SCODE_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if l, ok := parseLang(os.Getenv(env)); ok {
			return l
		}
	}
	if l, ok := osLang(); ok {
		return l
	}
	return En
}

// parseLang maps a locale string ("zh", "zh_CN.UTF-8", "en-US") onto a
// Lang; ok=false for empty or unsupported values.
func parseLang(s string) (Lang, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return En, false
	}
	// Strip codeset/modifier suffixes, normalize separators.
	if i := strings.IndexAny(s, ".@"); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, "_", "-")
	switch strings.SplitN(s, "-", 2)[0] {
	case "zh":
		return Zh, true
	case "en":
		return En, true
	}
	return En, false
}
