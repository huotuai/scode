import { create } from 'zustand';

export type Theme = 'light' | 'dark' | 'system';
// Code highlight palettes: 'one' = the built-in One Light/One Dark tokens,
// 'github' = GitHub Light/Dark overrides (see tokens.css).
export type CodeTheme = 'github' | 'one';
// Settings dialog pages; openSettings can land on a specific one.
export type SettingsSection = 'appearance' | 'models';

const THEME_KEY = 'scode.theme';
const SIDEBAR_KEY = 'scode.sidebarCollapsed';
const BROWSER_W_KEY = 'scode.browserWidth';
const BROWSER_W_DEFAULT = 480;
const BROWSER_W_MIN = 320;

// appearance settings (设置 → 外观)
const UI_FONT_KEY = 'scode.uiFontSize';
const CODE_FONT_KEY = 'scode.codeFontSize';
const CODE_THEME_LIGHT_KEY = 'scode.codeThemeLight';
const CODE_THEME_DARK_KEY = 'scode.codeThemeDark';
const CODE_LINES_KEY = 'scode.codeLineNumbers';
const CODE_WRAP_KEY = 'scode.codeWordWrap';

const UI_FONT_DEFAULT = 14;
const UI_FONT_MIN = 12;
const UI_FONT_MAX = 18;
const CODE_FONT_DEFAULT = 12;
const CODE_FONT_MIN = 10;
const CODE_FONT_MAX = 16;

function loadBrowserWidth(): number {
  const n = Number(localStorage.getItem(BROWSER_W_KEY));
  return Number.isFinite(n) && n >= BROWSER_W_MIN ? n : BROWSER_W_DEFAULT;
}

function clampBrowserWidth(w: number): number {
  const max = Math.max(BROWSER_W_MIN, Math.round(window.innerWidth * 0.75));
  return Math.min(Math.max(Math.round(w), BROWSER_W_MIN), max);
}

function loadTheme(): Theme {
  const v = localStorage.getItem(THEME_KEY);
  return v === 'dark' || v === 'system' ? v : 'light'; // light is the default
}

function loadNum(key: string, dflt: number, min: number, max: number): number {
  const n = Number(localStorage.getItem(key));
  return Number.isFinite(n) && n >= min && n <= max ? Math.round(n) : dflt;
}

function loadBool(key: string, dflt: boolean): boolean {
  const v = localStorage.getItem(key);
  return v === null ? dflt : v === '1';
}

function loadCodeTheme(key: string): CodeTheme {
  return localStorage.getItem(key) === 'one' ? 'one' : 'github';
}

const systemDark = () => window.matchMedia('(prefers-color-scheme: dark)').matches;

interface Appearance {
  theme: Theme;
  uiFontSize: number;
  codeThemeLight: CodeTheme;
  codeThemeDark: CodeTheme;
  codeLineNumbers: boolean;
  codeWordWrap: boolean;
  codeFontSize: number;
}

// applyAppearance pushes the settings onto documentElement: data-theme is
// the RESOLVED theme ('system' follows the OS), data-code-theme the code
// palette for that resolved theme, and the two font sizes become CSS vars
// (--ui-font-size / --code-font-size in tokens.css).
function applyAppearance(a: Appearance) {
  const root = document.documentElement;
  const dark = a.theme === 'dark' || (a.theme === 'system' && systemDark());
  root.dataset.theme = dark ? 'dark' : 'light';
  root.dataset.codeTheme = dark ? a.codeThemeDark : a.codeThemeLight;
  root.dataset.codeLines = a.codeLineNumbers ? '1' : '0';
  root.dataset.codeWrap = a.codeWordWrap ? '1' : '0';
  root.style.setProperty('--ui-font-size', `${a.uiFontSize}px`);
  root.style.setProperty('--code-font-size', `${a.codeFontSize}px`);
  localStorage.setItem(THEME_KEY, a.theme);
  localStorage.setItem(UI_FONT_KEY, String(a.uiFontSize));
  localStorage.setItem(CODE_FONT_KEY, String(a.codeFontSize));
  localStorage.setItem(CODE_THEME_LIGHT_KEY, a.codeThemeLight);
  localStorage.setItem(CODE_THEME_DARK_KEY, a.codeThemeDark);
  localStorage.setItem(CODE_LINES_KEY, a.codeLineNumbers ? '1' : '0');
  localStorage.setItem(CODE_WRAP_KEY, a.codeWordWrap ? '1' : '0');
}

interface UiState extends Appearance {
  sidebarCollapsed: boolean;
  settingsOpen: boolean;
  /** Active settings page; openSettings(section) lands there directly
   * (e.g. 管理模型 opens the models page). */
  settingsSection: SettingsSection;
  // Right-side in-app browser: session links open here (a <webview>
  // guest), never by navigating the chat UI away. null = panel closed.
  browserUrl: string | null;
  browserWidth: number;
  toggleSidebar(): void;
  setSettingsOpen(open: boolean): void;
  openSettings(section?: SettingsSection): void;
  setSettingsSection(section: SettingsSection): void;
  openBrowser(url: string): void;
  closeBrowser(): void;
  setBrowserWidth(w: number): void;
  setTheme(t: Theme): void;
  setUiFontSize(n: number): void;
  setCodeThemeLight(t: CodeTheme): void;
  setCodeThemeDark(t: CodeTheme): void;
  setCodeLineNumbers(on: boolean): void;
  setCodeWordWrap(on: boolean): void;
  setCodeFontSize(n: number): void;
}

export const useUiStore = create<UiState>((set, get) => {
  // Appearance setters share one path: patch state, re-apply + persist.
  const patchAppearance = (p: Partial<Appearance>) => {
    set(p);
    const s = get();
    applyAppearance({
      theme: s.theme,
      uiFontSize: s.uiFontSize,
      codeThemeLight: s.codeThemeLight,
      codeThemeDark: s.codeThemeDark,
      codeLineNumbers: s.codeLineNumbers,
      codeWordWrap: s.codeWordWrap,
      codeFontSize: s.codeFontSize,
    });
  };

  return {
    theme: loadTheme(),
    uiFontSize: loadNum(UI_FONT_KEY, UI_FONT_DEFAULT, UI_FONT_MIN, UI_FONT_MAX),
    codeThemeLight: loadCodeTheme(CODE_THEME_LIGHT_KEY),
    codeThemeDark: loadCodeTheme(CODE_THEME_DARK_KEY),
    codeLineNumbers: loadBool(CODE_LINES_KEY, true),
    codeWordWrap: loadBool(CODE_WRAP_KEY, false),
    codeFontSize: loadNum(CODE_FONT_KEY, CODE_FONT_DEFAULT, CODE_FONT_MIN, CODE_FONT_MAX),
    sidebarCollapsed: localStorage.getItem(SIDEBAR_KEY) === '1',
    settingsOpen: false,
    settingsSection: 'appearance',
    browserUrl: null,
    browserWidth: loadBrowserWidth(),
    toggleSidebar() {
      const sidebarCollapsed = !get().sidebarCollapsed;
      localStorage.setItem(SIDEBAR_KEY, sidebarCollapsed ? '1' : '0');
      set({ sidebarCollapsed });
    },
    setSettingsOpen: settingsOpen => set({ settingsOpen }),
    openSettings(section) {
      // An explicit section wins; otherwise the dialog reopens on the
      // page it was last showing.
      set(section ? { settingsOpen: true, settingsSection: section } : { settingsOpen: true });
    },
    setSettingsSection: settingsSection => set({ settingsSection }),
    openBrowser(url) {
      // Only web pages belong in the panel; other schemes stay external.
      if (!/^https?:\/\//i.test(url)) return;
      set({ browserUrl: url });
    },
    closeBrowser: () => set({ browserUrl: null }),
    setBrowserWidth(w) {
      const browserWidth = clampBrowserWidth(w);
      localStorage.setItem(BROWSER_W_KEY, String(browserWidth));
      set({ browserWidth });
    },
    setTheme: theme => patchAppearance({ theme }),
    setUiFontSize: n =>
      patchAppearance({
        uiFontSize: Math.min(UI_FONT_MAX, Math.max(UI_FONT_MIN, Math.round(n))),
      }),
    setCodeThemeLight: codeThemeLight => patchAppearance({ codeThemeLight }),
    setCodeThemeDark: codeThemeDark => patchAppearance({ codeThemeDark }),
    setCodeLineNumbers: codeLineNumbers => patchAppearance({ codeLineNumbers }),
    setCodeWordWrap: codeWordWrap => patchAppearance({ codeWordWrap }),
    setCodeFontSize: n =>
      patchAppearance({
        codeFontSize: Math.min(CODE_FONT_MAX, Math.max(CODE_FONT_MIN, Math.round(n))),
      }),
  };
});

// Apply the persisted appearance before first paint (module side effect),
// and track the OS theme while the 'system' option is selected.
{
  const s = useUiStore.getState();
  applyAppearance(s);
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
    if (useUiStore.getState().theme === 'system') {
      const cur = useUiStore.getState();
      applyAppearance(cur);
    }
  });
}
