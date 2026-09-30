//go:build windows

package i18n

import "golang.org/x/sys/windows"

// muiLanguageName asks for "zh-CN"-style names.
const muiLanguageName = 0x8

// osLang reads the Windows user UI language preference.
func osLang() (Lang, bool) {
	langs, err := windows.GetUserPreferredUILanguages(muiLanguageName)
	if err != nil || len(langs) == 0 {
		return En, false
	}
	return parseLang(langs[0])
}
