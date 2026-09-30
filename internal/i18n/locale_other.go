//go:build !windows

package i18n

// osLang has no platform source outside Windows; LANG-family env vars
// (checked in detect) are the unix convention.
func osLang() (Lang, bool) { return En, false }
