import { create } from 'zustand';
import type { ModelProfile, ModelsListResult } from '../types';
import { rpc, errText } from '../lib/rpc';
import { useSessionStore, patchRuntime } from './session';
import { useChatStore, DRAFT_KEY } from './chat';
import { t, thinkingLabel } from '../i18n';

// Reasoning-effort display names live in i18n.thinkingLabel (localized).

interface ModelsState {
  models: { label: string; provider: string; model: string; reasoning?: boolean }[];
  profiles: ModelProfile[];
  defaultChoice: { provider: string; model: string };
  /** settings 级别的默认推理强度('' = provider 默认) */
  defaultThinking: string;
  loaded: boolean;
  refresh(): Promise<void>;
  switchModel(provider: string, model: string): Promise<void>;
  switchThinking(level: string): Promise<void>;
  saveProfile(p: ModelProfile & { original?: string }): Promise<void>;
  deleteProfile(name: string): Promise<void>;
  setDefault(name: string, model?: string): Promise<void>;
}

export const useModelsStore = create<ModelsState>((set, get) => ({
  models: [],
  profiles: [],
  defaultChoice: { provider: '', model: '' },
  defaultThinking: '',
  loaded: false,

  async refresh() {
    try {
      const r = await rpc<ModelsListResult>('models/list', {});
      set({
        models: r.models || [],
        profiles: r.profiles || [],
        defaultChoice: r.default || { provider: '', model: '' },
        defaultThinking: r.thinking || '',
        loaded: true,
      });
    } catch {
      // The selector is optional; the free-form input still works.
      set({ loaded: true });
    }
  },

  async switchModel(provider, model) {
    if (!model) return;
    const chat = useChatStore.getState();
    let sessionId: string;
    try {
      sessionId = await useSessionStore.getState().ensureSession();
    } catch (err) {
      chat.addError(useSessionStore.getState().sessionId ?? DRAFT_KEY, errText(err));
      return;
    }
    // A draft has no provider yet: inherit the one the new session resolved.
    if (!provider) {
      provider = useSessionStore.getState().runtimes[sessionId]?.provider || '';
    }
    try {
      const st = await rpc<{ provider: string; model: string }>('session/model', {
        sessionId,
        provider,
        model,
      });
      patchRuntime(sessionId, { provider: st.provider, model: st.model });
      chat.addNote(sessionId, t('store.modelSwitched', { provider: st.provider, model: st.model }));
    } catch (err) {
      chat.addError(sessionId, errText(err));
      throw err;
    }
  },

  async switchThinking(level) {
    const chat = useChatStore.getState();
    let sessionId: string;
    try {
      sessionId = await useSessionStore.getState().ensureSession();
    } catch (err) {
      chat.addError(useSessionStore.getState().sessionId ?? DRAFT_KEY, errText(err));
      return;
    }
    try {
      const st = await rpc<{ thinking: string }>('session/thinking', {
        sessionId,
        level,
      });
      patchRuntime(sessionId, { thinking: st.thinking ?? '' });
      chat.addNote(sessionId, t('store.thinkingSwitched', { label: thinkingLabel(st.thinking ?? '') }));
    } catch (err) {
      chat.addError(sessionId, errText(err));
      throw err;
    }
  },

  async saveProfile(p) {
    await rpc('models/save', p);
    await get().refresh();
  },

  async deleteProfile(name) {
    await rpc('models/delete', { name });
    await get().refresh();
  },

  async setDefault(name, model) {
    await rpc('models/default', { provider: name, model: model || '' });
    await get().refresh();
  },
}));
