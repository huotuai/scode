import { useEffect, useRef, useState, type ReactNode } from 'react';
import { ChevronDown, Monitor, Moon, Sun } from 'lucide-react';
import { useUiStore, type CodeTheme, type Theme } from '../../store/ui';

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

const THEME_OPTIONS: Option<Theme>[] = [
  { value: 'light', label: '浅色', icon: <Sun size={13} /> },
  { value: 'dark', label: '深色', icon: <Moon size={13} /> },
  { value: 'system', label: '跟随系统', icon: <Monitor size={13} /> },
];

const LIGHT_CODE_OPTIONS: Option<CodeTheme>[] = [
  { value: 'github', label: 'GitHub Light' },
  { value: 'one', label: 'One Light' },
];
const DARK_CODE_OPTIONS: Option<CodeTheme>[] = [
  { value: 'github', label: 'GitHub Dark' },
  { value: 'one', label: 'One Dark' },
];

export default function AppearanceSettings() {
  const s = useUiStore();
  return (
    <div className="appr">
      <div className="appr-card">
        <Row title="界面主题" desc="选择浅色、深色或跟随系统主题。">
          <Select value={s.theme} options={THEME_OPTIONS} onChange={s.setTheme} />
        </Row>
        <Row title="界面字号" desc="调整应用界面的文字大小,图标和布局尺寸不受影响。">
          <NumberField value={s.uiFontSize} onCommit={s.setUiFontSize} />
        </Row>
      </div>

      <div className="appr-group-title">代码设置</div>
      <div className="appr-group-desc">设置代码内容的主题、字号和显示方式,不受界面字号影响。</div>
      <div className="appr-card">
        <Row title="浅色代码主题" desc="浅色界面下代码内容使用的高亮主题。">
          <Select
            value={s.codeThemeLight}
            options={LIGHT_CODE_OPTIONS}
            onChange={s.setCodeThemeLight}
          />
        </Row>
        <Row title="深色代码主题" desc="深色界面下代码内容使用的高亮主题。">
          <Select
            value={s.codeThemeDark}
            options={DARK_CODE_OPTIONS}
            onChange={s.setCodeThemeDark}
          />
        </Row>
        <Row title="显示行号" desc="在代码内容和差异视图中显示行号。">
          <Switch checked={s.codeLineNumbers} onChange={s.setCodeLineNumbers} />
        </Row>
        <Row title="长行自动换行" desc="代码内容过长时自动换行。">
          <Switch checked={s.codeWordWrap} onChange={s.setCodeWordWrap} />
        </Row>
        <Row title="代码字号" desc="调整代码块、文件预览和差异视图的默认字号。">
          <NumberField value={s.codeFontSize} onCommit={s.setCodeFontSize} />
        </Row>
      </div>
    </div>
  );
}
