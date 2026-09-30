import { useEffect, useState } from 'react';
import { Plus, Star, Pencil, Trash2 } from 'lucide-react';
import type { ModelProfile } from '../../types';
import { useModelsStore } from '../../store/models';
import { errText } from '../../lib/rpc';
import { useT } from '../../i18n';

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
  const t = useT();
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
    setStatus(t('modelSettings.newProfile'));
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
    setStatus(t('modelSettings.editing', { name: p.name }));
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
      setStatus(t('modelSettings.nameRequired'));
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
      setStatus(t('modelSettings.saved', { name }));
      setDirty(false);
    } catch (err) {
      setStatus(errText(err));
    }
  };

  const onDelete = async (name: string) => {
    if (!confirm(t('modelSettings.confirmDelete', { name }))) return;
    try {
      await deleteProfile(name);
      if (editingName === name) clearForm();
      setStatus(t('modelSettings.deleted', { name }));
    } catch (err) {
      setStatus(errText(err));
    }
  };

  const onSetDefault = async (p: ModelProfile) => {
    try {
      await setDefault(p.name, p.model);
      setStatus(t('modelSettings.setDefaultDone', { name: p.name }));
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
        <span className="profile-list-title">{t('modelSettings.title')}</span>
        <span className="spacer" />
        <button className="btn sm" onClick={clearForm}>
          <Plus size={13} />
          {t('common.new')}
        </button>
      </div>
      <div className="profile-list">
        {profiles.length === 0 && <div className="dim">{t('modelSettings.empty')}</div>}
        {profiles.map(p => (
          <div key={p.name} className="profile-row">
            <div className="profile-info">
              <div className="profile-name">
                {p.name}
                {p.name === defaultChoice.provider && <span className="badge accent">{t('modelSettings.defaultBadge')}</span>}
              </div>
              <div className="dim profile-sub">
                {p.protocol || 'openai-compat'} · {p.model || t('modelSettings.defaultModelMark')}
                {p.baseUrl ? ` · ${p.baseUrl}` : ''}
              </div>
            </div>
            <div className="profile-actions">
              <button className="btn sm" onClick={() => onSetDefault(p)} title={t('modelSettings.setDefault')}>
                <Star size={12} />
                {t('modelSettings.setDefault')}
              </button>
              <button className="btn sm" onClick={() => fillForm(p)}>
                <Pencil size={12} />
                {t('common.edit')}
              </button>
              <button className="btn sm danger-ghost" onClick={() => onDelete(p.name)}>
                <Trash2 size={12} />
                {t('common.delete')}
              </button>
            </div>
          </div>
        ))}
      </div>

      <form onSubmit={onSave} className="profile-form" autoComplete="off">
        <div className="form-grid">
          <label>
            {t('modelSettings.nameLabel')}
            <input value={form.name} onChange={set('name')} placeholder={t('modelSettings.namePlaceholder')} />
          </label>
          <label>
            {t('modelSettings.protocolLabel')}
            <select value={form.protocol} onChange={set('protocol')}>
              <option value="">{t('modelSettings.protocolDefault')}</option>
              <option value="openai-compat">openai-compat</option>
              <option value="openai-responses">openai-responses</option>
              <option value="azure-openai-responses">azure-openai-responses</option>
              <option value="google">google</option>
              <option value="anthropic">anthropic</option>
            </select>
          </label>
          <label>
            {t('modelSettings.modelIdLabel')}
            <input value={form.model} onChange={set('model')} placeholder={t('modelSettings.namePlaceholder')} />
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
            {t('modelSettings.imageInput')}
            <select value={form.imageInput} onChange={set('imageInput')}>
              <option value="">{t('modelSettings.optionDefault')}</option>
              <option value="true">{t('modelSettings.optionYes')}</option>
              <option value="false">{t('modelSettings.optionNo')}</option>
            </select>
          </label>
          <label className="check">
            <input type="checkbox" checked={form.reasoning} onChange={set('reasoning')} />
            {t('modelSettings.reasoning')}
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
          <button type="submit" className="btn primary">{t('common.save')}</button>
          <button type="button" className="btn" onClick={clearForm}>{t('common.clear')}</button>
          <span className="dim">{status}</span>
        </div>
      </form>
    </>
  );
}
