import { useEffect, useState } from 'react';
import { Plus, Star, Pencil, Trash2 } from 'lucide-react';
import type { ModelProfile } from '../../types';
import { useModelsStore } from '../../store/models';
import { errText } from '../../lib/rpc';

interface FormState {
  name: string;
  protocol: string;
  model: string;
  baseUrl: string;
  apiKey: string;
  contextWindow: string;
  maxTokens: string;
  imageInput: string; // '' | 'true' | 'false'
  reasoning: boolean;
  priceInput: string;
  priceOutput: string;
  priceRead: string;
  priceWrite: string;
}

const emptyForm: FormState = {
  name: '',
  protocol: '',
  model: '',
  baseUrl: '',
  apiKey: '',
  contextWindow: '',
  maxTokens: '',
  imageInput: '',
  reasoning: false,
  priceInput: '',
  priceOutput: '',
  priceRead: '',
  priceWrite: '',
};

function toForm(p: ModelProfile): FormState {
  return {
    name: p.name || '',
    protocol: p.protocol || '',
    model: p.model || '',
    baseUrl: p.baseUrl || '',
    apiKey: p.apiKey || '',
    contextWindow: p.contextWindow ? String(p.contextWindow) : '',
    maxTokens: p.maxTokens ? String(p.maxTokens) : '',
    imageInput: p.imageInput === true ? 'true' : p.imageInput === false ? 'false' : '',
    reasoning: !!p.reasoning,
    priceInput: p.pricing?.input ? String(p.pricing.input) : '',
    priceOutput: p.pricing?.output ? String(p.pricing.output) : '',
    priceRead: p.pricing?.cacheRead ? String(p.pricing.cacheRead) : '',
    priceWrite: p.pricing?.cacheWrite ? String(p.pricing.cacheWrite) : '',
  };
}

interface Props {
  // Reports unsaved-edits state to the host dialog so closing can confirm
  // instead of silently dropping them.
  onDirtyChange?: (dirty: boolean) => void;
}

// Model settings panel, embedded as a section of SettingsDialog (the
// dialog owns the modal frame and close confirmation).
export default function ModelSettings({ onDirtyChange }: Props) {
  const profiles = useModelsStore(s => s.profiles);
  const defaultChoice = useModelsStore(s => s.defaultChoice);
  const refresh = useModelsStore(s => s.refresh);
  const saveProfile = useModelsStore(s => s.saveProfile);
  const deleteProfile = useModelsStore(s => s.deleteProfile);
  const setDefault = useModelsStore(s => s.setDefault);

  const [form, setForm] = useState<FormState>(emptyForm);
  const [editingName, setEditingName] = useState(''); // '' = creating
  const [status, setStatus] = useState('');
  const [dirty, setDirty] = useState(false);

  const clearForm = () => {
    setForm(emptyForm);
    setEditingName('');
    setStatus('新建模型配置');
    setDirty(false);
  };

  // The panel mounts when the dialog opens (and unmounts on close), so a
  // mount effect is the refresh/reset point.
  useEffect(() => {
    refresh();
    clearForm();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);

  const fillForm = (p: ModelProfile) => {
    setForm(toForm(p));
    setEditingName(p.name);
    setStatus(`编辑中:${p.name}`);
    setDirty(false);
  };

  const num = (s: string) => {
    const n = Number(s);
    return Number.isFinite(n) ? n : 0;
  };

  const onSave = async (e: React.FormEvent) => {
    e.preventDefault();
    const name = form.name.trim();
    if (!name) {
      setStatus('名称必填');
      return;
    }
    const payload: ModelProfile & { original?: string } = {
      name,
      original: editingName || '',
      protocol: form.protocol,
      model: form.model.trim(),
      baseUrl: form.baseUrl.trim(),
      apiKey: form.apiKey,
      contextWindow: num(form.contextWindow),
      maxTokens: num(form.maxTokens),
      reasoning: form.reasoning,
    };
    if (form.imageInput === 'true') payload.imageInput = true;
    else if (form.imageInput === 'false') payload.imageInput = false;
    const pricing = {
      input: num(form.priceInput),
      output: num(form.priceOutput),
      cacheRead: num(form.priceRead),
      cacheWrite: num(form.priceWrite),
    };
    if (pricing.input || pricing.output || pricing.cacheRead || pricing.cacheWrite) {
      payload.pricing = pricing;
    }
    try {
      await saveProfile(payload);
      setEditingName(name);
      setStatus(`已保存:${name}`);
      setDirty(false);
    } catch (err) {
      setStatus(errText(err));
    }
  };

  const onDelete = async (name: string) => {
    if (!confirm(`删除模型配置「${name}」?`)) return;
    try {
      await deleteProfile(name);
      if (editingName === name) clearForm();
      setStatus(`已删除:${name}`);
    } catch (err) {
      setStatus(errText(err));
    }
  };

  const onSetDefault = async (p: ModelProfile) => {
    try {
      await setDefault(p.name, p.model);
      setStatus(`已设为默认:${p.name}`);
    } catch (err) {
      setStatus(errText(err));
    }
  };

  const set = (k: keyof FormState) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => {
    setDirty(true);
    setForm(f => ({
      ...f,
      [k]: e.target.type === 'checkbox' ? (e.target as HTMLInputElement).checked : e.target.value,
    }));
  };

  return (
    <>
      <div className="profile-list-head">
        <span className="profile-list-title">模型配置</span>
        <span className="spacer" />
        <button className="btn sm" onClick={clearForm}>
          <Plus size={13} />
          新建
        </button>
      </div>
      <div className="profile-list">
        {profiles.length === 0 && <div className="dim">暂无模型配置,点击"新建"添加。</div>}
        {profiles.map(p => (
          <div key={p.name} className="profile-row">
            <div className="profile-info">
              <div className="profile-name">
                {p.name}
                {p.name === defaultChoice.provider && <span className="badge accent">默认</span>}
              </div>
              <div className="dim profile-sub">
                {p.protocol || 'openai-compat'} · {p.model || '(默认模型)'}
                {p.baseUrl ? ` · ${p.baseUrl}` : ''}
              </div>
            </div>
            <div className="profile-actions">
              <button className="btn sm" onClick={() => onSetDefault(p)} title="设为默认">
                <Star size={12} />
                设为默认
              </button>
              <button className="btn sm" onClick={() => fillForm(p)}>
                <Pencil size={12} />
                编辑
              </button>
              <button className="btn sm danger-ghost" onClick={() => onDelete(p.name)}>
                <Trash2 size={12} />
                删除
              </button>
            </div>
          </div>
        ))}
      </div>

      <form onSubmit={onSave} className="profile-form" autoComplete="off">
        <div className="form-grid">
          <label>
            名称
            <input value={form.name} onChange={set('name')} placeholder="如 kimi" />
          </label>
          <label>
            协议
            <select value={form.protocol} onChange={set('protocol')}>
              <option value="">默认(openai-compat)</option>
              <option value="openai-compat">openai-compat</option>
              <option value="openai-responses">openai-responses</option>
              <option value="azure-openai-responses">azure-openai-responses</option>
              <option value="google">google</option>
              <option value="anthropic">anthropic</option>
            </select>
          </label>
          <label>
            模型 ID
            <input value={form.model} onChange={set('model')} placeholder="如 k3" />
          </label>
          <label>
            baseUrl
            <input value={form.baseUrl} onChange={set('baseUrl')} placeholder="https://…/v1" />
          </label>
          <label>
            apiKey
            <input type="password" value={form.apiKey} onChange={set('apiKey')} />
          </label>
          <label>
            contextWindow
            <input type="number" min={0} value={form.contextWindow} onChange={set('contextWindow')} />
          </label>
          <label>
            maxTokens
            <input type="number" min={0} value={form.maxTokens} onChange={set('maxTokens')} />
          </label>
          <label>
            图片输入
            <select value={form.imageInput} onChange={set('imageInput')}>
              <option value="">默认</option>
              <option value="true">支持</option>
              <option value="false">不支持</option>
            </select>
          </label>
          <label className="check">
            <input type="checkbox" checked={form.reasoning} onChange={set('reasoning')} />
            支持思考(reasoning)
          </label>
        </div>
        <div className="form-grid pricing">
          <label>
            input $/M
            <input type="number" step="0.01" min={0} value={form.priceInput} onChange={set('priceInput')} />
          </label>
          <label>
            output $/M
            <input type="number" step="0.01" min={0} value={form.priceOutput} onChange={set('priceOutput')} />
          </label>
          <label>
            cacheRead $/M
            <input type="number" step="0.01" min={0} value={form.priceRead} onChange={set('priceRead')} />
          </label>
          <label>
            cacheWrite $/M
            <input type="number" step="0.01" min={0} value={form.priceWrite} onChange={set('priceWrite')} />
          </label>
        </div>
        <div className="form-foot">
          <button type="submit" className="btn primary">保存</button>
          <button type="button" className="btn" onClick={clearForm}>清空</button>
          <span className="dim">{status}</span>
        </div>
      </form>
    </>
  );
}
