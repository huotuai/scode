import { useEffect, useRef, useState, type ReactNode } from 'react';
import { ChevronDown, Monitor, Moon, Sun } from 'lucide-react';
import { useUiStore, type CodeTheme, type Theme } from '../../store/ui';
import { useI18n, useT, type Lang } from '../../i18n';

// Appearance settings (设置 → 外观): every option applies immediately and
// persists to localStorage via the ui store — there is no dirty/save flow
// like the model profiles.

// Row: label + description on the left, control on the right.
function Row({ title, desc, children }: { title: string; desc: string; children: ReactNode }) {
  return (
    <div className="appr-row">
      <div className="appr-row-text">
        <div className="appr-row-title">{title}</div>
        <div className="appr-row-desc">{desc}</div>
      </div>
      {children}
    </div>
  );
}

interface Option<T> {
  value: T;
  label: string;
  icon?: ReactNode;
}

// Select: a small down-popup in the model-menu style (native <select>
// can't carry leading icons).
function Select<T extends string>({
  value,
  options,
  onChange,
}: {
  value: T;
  options: Option<T>[];
  onChange: (v: T) => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, [open]);

  const current = options.find(o => o.value === value) ?? options[0];

  return (
    <div className="appr-select" ref={ref}>
      <button className="appr-select-btn" onClick={() => setOpen(o => !o)}>
        {current.icon}
        <span>{current.label}</span>
        <ChevronDown size={13} className="dim" />
      </button>
      {open && (
        <div className="appr-select-menu">
          {options.map(o => (
            <button
              key={o.value}
              className={`appr-select-option${o.value === value ? ' active' : ''}`}
              onClick={() => {
                setOpen(false);
                if (o.value !== value) onChange(o.value);
              }}
            >
              {o.icon}
              {o.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function Switch({ checked, onChange }: { checked: boolean; onChange: (on: boolean) => void }) {
  return (
    <button
      className={`appr-switch${checked ? ' on' : ''}`}
      role="switch"
      aria-checked={checked}
      onClick={() => onChange(!checked)}
    >
      <span className="appr-switch-dot" />
    </button>
  );
}

// NumberField: px-valued input committing on Enter/blur (clamped by the
// store). Local state lets the user type intermediate values freely.
function NumberField({
  value,
  onCommit,
}: {
  value: number;
  onCommit: (n: number) => void;
}) {
  const [text, setText] = useState(String(value));
  useEffect(() => setText(String(value)), [value]);
  const commit = () => {
    const n = Number(text);
    if (Number.isFinite(n)) onCommit(n);
    else setText(String(value));
  };
  return (
    <span className="appr-num">
      <input
        type="number"
        value={text}
        onChange={e => setText(e.target.value)}
        onBlur={commit}
        onKeyDown={e => {
          if (e.key === 'Enter') {
            e.preventDefault();
            commit();
          }
        }}
      />
      <span className="appr-num-unit">px</span>
    </span>
  );
}

// Options resolve per render so a language switch re-labels them.
function themeOptions(t: ReturnType<typeof useT>): Option<Theme>[] {
  return [
    { value: 'light', label: t('appearance.light'), icon: <Sun size={13} /> },
    { value: 'dark', label: t('appearance.dark'), icon: <Moon size={13} /> },
    { value: 'system', label: t('appearance.system'), icon: <Monitor size={13} /> },
  ];
}

type LangChoice = Lang | 'system';

const LIGHT_CODE_OPTIONS: Option<CodeTheme>[] = [
  { value: 'github', label: 'GitHub Light' },
  { value: 'one', label: 'One Light' },
];
const DARK_CODE_OPTIONS: Option<CodeTheme>[] = [
  { value: 'github', label: 'GitHub Dark' },
  { value: 'one', label: 'One Dark' },
];

export default function AppearanceSettings() {
  const t = useT();
  const s = useUiStore();
  const lang = useI18n(st => st.lang);
  const setLang = useI18n(st => st.setLang);
  // The select shows the OVERRIDE when one is persisted, else 'system'.
  const langOverride = (() => {
    try {
      const v = localStorage.getItem('scode-lang');
      return v === 'zh' || v === 'en' ? (v as LangChoice) : 'system';
    } catch {
      return 'system';
    }
  })();
  const langOptions: Option<LangChoice>[] = [
    { value: 'system', label: t('appearance.system') },
    { value: 'zh', label: t('appearance.langZh') },
    { value: 'en', label: t('appearance.langEn') },
  ];
  void lang; // the selector above subscribes for re-render
  return (
    <div className="appr">
      <div className="appr-card">
        <Row title={t('appearance.themeRow')} desc={t('appearance.themeDesc')}>
          <Select value={s.theme} options={themeOptions(t)} onChange={s.setTheme} />
        </Row>
        <Row title={t('appearance.langRow')} desc={t('appearance.langDesc')}>
          <Select value={langOverride} options={langOptions} onChange={setLang} />
        </Row>
        <Row title={t('appearance.fontRow')} desc={t('appearance.fontDesc')}>
          <NumberField value={s.uiFontSize} onCommit={s.setUiFontSize} />
        </Row>
      </div>

      <div className="appr-group-title">{t('appearance.codeGroup')}</div>
      <div className="appr-group-desc">{t('appearance.codeGroupDesc')}</div>
      <div className="appr-card">
        <Row title={t('appearance.lightCode')} desc={t('appearance.lightCodeDesc')}>
          <Select
            value={s.codeThemeLight}
            options={LIGHT_CODE_OPTIONS}
            onChange={s.setCodeThemeLight}
          />
        </Row>
        <Row title={t('appearance.darkCode')} desc={t('appearance.darkCodeDesc')}>
          <Select
            value={s.codeThemeDark}
            options={DARK_CODE_OPTIONS}
            onChange={s.setCodeThemeDark}
          />
        </Row>
        <Row title={t('appearance.lineNumbers')} desc={t('appearance.lineNumbersDesc')}>
          <Switch checked={s.codeLineNumbers} onChange={s.setCodeLineNumbers} />
        </Row>
        <Row title={t('appearance.wrap')} desc={t('appearance.wrapDesc')}>
          <Switch checked={s.codeWordWrap} onChange={s.setCodeWordWrap} />
        </Row>
        <Row title={t('appearance.codeFontSize')} desc={t('appearance.codeFontSizeDesc')}>
          <NumberField value={s.codeFontSize} onCommit={s.setCodeFontSize} />
        </Row>
      </div>
    </div>
  );
}
