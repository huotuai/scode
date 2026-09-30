import { create } from 'zustand';
import type { ChatItem, SessionEvent, TranscriptMessage } from '../types';
// Aliased: replay loops name their task/item variables `t`.
import { t as i18nT } from '../i18n';

// ---------------------------------------------------------------------------
// multi-session chat state
//
// Every open session keeps its own item list keyed by session id, so a run
// in flight keeps streaming into its own log while the user looks at (and
// works in) another session. The unsent draft uses DRAFT_KEY.
// ---------------------------------------------------------------------------

export const DRAFT_KEY = '';

let nextKey = 1;
const key = () => `m${nextKey++}`;

// Per-session streaming plumbing. LLM deltas arrive at token frequency;
// rendering per token would thrash React, so deltas accumulate in the
// session's buffer and flush into the store at most once per animation
// frame. Live item keys for the in-flight stream (the old renderer's
// streamBubble / thinkBubble lifecycles) are per session too.
interface StreamCtx {
  streamKey: string | null; // assistant text bubble
  thinkKey: string | null; // thinking bubble (anchored above stream)
  pending: { text: string; textKey: string | null; thinking: string; thinkKey: string | null };
  flushScheduled: boolean;
}

const streams = new Map<string, StreamCtx>();

function ctxFor(k: string): StreamCtx {
  let c = streams.get(k);
  if (!c) {
    c = {
      streamKey: null,
      thinkKey: null,
      pending: { text: '', textKey: null, thinking: '', thinkKey: null },
      flushScheduled: false,
    };
    streams.set(k, c);
  }
  return c;
}

function resetCtx(c: StreamCtx) {
  c.streamKey = null;
  c.thinkKey = null;
  c.pending.text = '';
  c.pending.textKey = null;
  c.pending.thinking = '';
  c.pending.thinkKey = null;
}

// Each buffer remembers the item key it belongs to, so a terminal event
// (text_end, turn_end, …) that clears streamKey cannot strand buffered
// text: the rAF flush still targets the right bubble. rAF is paused while
// the window is hidden, so whole paragraphs can be buffered — dropping
// them on the terminal event was the "lost text" bug.

function abbrev(s: unknown, n: number): string {
  const str = String(s ?? '');
  return str.length > n ? str.slice(0, n) + '…' : str;
}

interface ChatState {
  bySession: Record<string, ChatItem[]>;
  reset(k: string): void;
  /** Drop a deleted session's log and stream state. */
  drop(k: string): void;
  /** Move the draft log onto a freshly created session id. */
  migrate(from: string, to: string): void;
  addUser(k: string, text: string, images?: string[]): void;
  addSteer(k: string, text: string): void;
  addError(k: string, text: string): void;
  addNote(k: string, text: string): void;
  handleEvent(ev: SessionEvent): void;
  loadTranscript(k: string, messages: TranscriptMessage[]): void;
}

export const useChatStore = create<ChatState>((set, get) => {
  function push(k: string, item: ChatItem) {
    set(s => ({ bySession: { ...s.bySession, [k]: [...(s.bySession[k] ?? []), item] } }));
  }

  // insertBefore inserts an item above an anchor key (thinking belongs
  // directly above its reply, whatever the event arrival order).
  function insertBefore(k: string, anchorKey: string | null, item: ChatItem) {
    set(s => {
      const items = [...(s.bySession[k] ?? [])];
      const idx = anchorKey ? items.findIndex(i => i.key === anchorKey) : -1;
      if (idx < 0) items.push(item);
      else items.splice(idx, 0, item);
      return { bySession: { ...s.bySession, [k]: items } };
    });
  }

  function patchItem(k: string, itemKey: string, patch: Partial<ChatItem>) {
    set(s => ({
      bySession: {
        ...s.bySession,
        [k]: (s.bySession[k] ?? []).map(i =>
          i.key === itemKey ? ({ ...i, ...patch } as ChatItem) : i,
        ),
      },
    }));
  }

  function flushDeltas(k: string) {
    const c = ctxFor(k);
    c.flushScheduled = false;
    const text = c.pending.text;
    const textKey = c.pending.textKey;
    const thinking = c.pending.thinking;
    const thinkingKey = c.pending.thinkKey;
    c.pending.text = '';
    c.pending.textKey = null;
    c.pending.thinking = '';
    c.pending.thinkKey = null;
    if (!text && !thinking) return;
    const items = get().bySession[k] ?? [];
    let changed = false;
    const next = items.map(i => {
      if (text && i.key === textKey && i.kind === 'assistant') {
        changed = true;
        return { ...i, text: i.text + text };
      }
      if (thinking && i.key === thinkingKey && i.kind === 'thinking') {
        changed = true;
        return { ...i, text: i.text + thinking };
      }
      return i;
    });
    if (changed) set(s => ({ bySession: { ...s.bySession, [k]: next } }));
  }

  function scheduleFlush(k: string) {
    const c = ctxFor(k);
    if (c.flushScheduled) return;
    c.flushScheduled = true;
    requestAnimationFrame(() => flushDeltas(k));
  }

  function appendTextDelta(k: string, delta: string) {
    const c = ctxFor(k);
    if (!c.streamKey) {
      const sk = key();
      c.streamKey = sk;
      push(k, { key: sk, kind: 'assistant', text: '', streaming: true });
    }
    c.pending.textKey = c.streamKey;
    c.pending.text += delta;
    scheduleFlush(k);
  }

  function appendThinkingDelta(k: string, delta: string) {
    const c = ctxFor(k);
    if (!c.thinkKey) {
      const tk = key();
      c.thinkKey = tk;
      insertBefore(k, c.streamKey, { key: tk, kind: 'thinking', text: '', streaming: true });
    }
    c.pending.thinkKey = c.thinkKey;
    c.pending.thinking += delta;
    scheduleFlush(k);
  }

  // endTextStream / endThinkingStream flush synchronously before the
  // stream key is cleared, so the final delta always lands (a rAF flush
  // scheduled moments before would otherwise run after streamKey is null
  // and find no target). The reply's completion time rides along — it
  // is what the per-reply action row shows.
  function endTextStream(k: string) {
    const c = ctxFor(k);
    flushDeltas(k);
    if (c.streamKey) patchItem(k, c.streamKey, { streaming: false, ts: Date.now() });
    c.streamKey = null;
  }

  function endThinkingStream(k: string) {
    const c = ctxFor(k);
    flushDeltas(k);
    if (c.thinkKey) patchItem(k, c.thinkKey, { streaming: false });
    c.thinkKey = null;
  }

  return {
    bySession: {},

    reset(k) {
      resetCtx(ctxFor(k));
      set(s => ({ bySession: { ...s.bySession, [k]: [] } }));
    },

    drop(k) {
      streams.delete(k);
      set(s => {
        const bySession = { ...s.bySession };
        delete bySession[k];
        return { bySession };
      });
    },

    migrate(from, to) {
      const c = streams.get(from);
      if (c) {
        streams.set(to, c);
        streams.delete(from);
      }
      set(s => {
        const items = s.bySession[from];
        if (!items || items.length === 0) return {};
        const bySession = { ...s.bySession, [to]: items };
        delete bySession[from];
        return { bySession };
      });
    },

    addUser: (k, text, images) =>
      push(k, { key: key(), kind: 'user', text, ...(images?.length ? { images } : {}) }),
    addSteer: (k, text) => push(k, { key: key(), kind: 'steer', text }),
    addError: (k, text) => push(k, { key: key(), kind: 'error', text }),
    addNote: (k, text) => push(k, { key: key(), kind: 'note', text }),

    handleEvent(ev) {
      const k = ev.sessionId || DRAFT_KEY;
      switch (ev.type) {
        case 'llm': {
          const lt = ev.llm?.type;
          if (lt === 'text_start' || lt === 'text_end') {
            endTextStream(k);
          } else if (lt === 'thinking_start' || lt === 'thinking_end') {
            endThinkingStream(k);
          } else if (lt === 'text_delta' && ev.llm?.delta) {
            appendTextDelta(k, ev.llm.delta);
          } else if (lt === 'thinking_delta' && ev.llm?.delta) {
            appendThinkingDelta(k, ev.llm.delta);
          }
          break;
        }
        case 'tool_start': {
          const call = ev.call;
          if (!call) break;
          push(k, {
            key: key(),
            kind: 'tool',
            callId: call.id,
            name: call.name,
            args: call.arguments,
            state: 'running',
          });
          break;
        }
        case 'tool_end': {
          const id = ev.call?.id;
          if (!id) break;
          const item = (get().bySession[k] ?? []).find(
            i => i.kind === 'tool' && i.callId === id,
          );
          if (item) {
            patchItem(k, item.key, {
              state: ev.result?.isError ? 'error' : 'ok',
              result: abbrev(ev.result?.text ?? '', 4000),
            });
          }
          break;
        }
        case 'user':
        case 'turn_end':
        case 'assistant':
          endTextStream(k);
          endThinkingStream(k);
          break;
        case 'agent_end':
          endTextStream(k);
          endThinkingStream(k);
          break;
        case 'agent_error':
        case 'run_error':
          endTextStream(k);
          endThinkingStream(k);
          push(k, { key: key(), kind: 'error', text: ev.error || 'run failed' });
          break;
      }
    },

    // Replays the projected transcript into chat items: thinking first
    // within a message (it logically precedes the reply), tool cards
    // paired with results by call id. Tool-result messages carry the
    // wire role "toolResult" (llm.RoleTool), not "tool".
    loadTranscript(k, messages) {
      const items: ChatItem[] = [];
      const toolIndex = new Map<string, ChatItem & { kind: 'tool' }>();
      for (const m of messages || []) {
        if (m.role === 'toolResult') {
          for (const b of m.content || []) {
            if (b.kind === 'toolResult') {
              const t = toolIndex.get(b.id);
              if (t) {
                t.state = b.isError ? 'error' : 'ok';
                t.result = abbrev((b.content || []).map(x => x.text || '').join(''), 4000);
              }
            }
          }
          continue;
        }
        const blocks = m.content || [];
        // Image blocks ride on user messages (pasted screenshots);
        // replay them as thumbnails on the user bubble. They attach to
        // the message's first text item, or a bubble of their own when
        // the prompt carried no text.
        let userImgs: string[] | undefined;
        if (m.role === 'user') {
          const imgs = blocks
            .filter(b => b.kind === 'image' && b.data)
            .map(b => `data:${(b as { mimeType?: string }).mimeType || 'image/png'};base64,${(b as { data?: string }).data}`);
          if (imgs.length > 0) userImgs = imgs;
        }
        let imgsAttached = false;
        for (const b of blocks) {
          if (b.kind === 'thinking' && b.text) {
            items.push({ key: key(), kind: 'thinking', text: b.text, streaming: false });
          }
        }
        for (const b of blocks) {
          if (b.kind === 'text' && b.text) {
            if (m.role === 'user') {
              const images = !imgsAttached ? userImgs : undefined;
              imgsAttached = true;
              items.push({ key: key(), kind: 'user', text: b.text, ...(images ? { images } : {}) });
            } else {
              items.push({ key: key(), kind: 'assistant', text: b.text, streaming: false, ...(m.ts ? { ts: m.ts } : {}) });
            }
          } else if (b.kind === 'toolCall') {
            const t: ChatItem & { kind: 'tool' } = {
              key: key(),
              kind: 'tool',
              callId: b.id,
              name: b.name,
              args: b.arguments,
              state: 'running',
            };
            items.push(t);
            if (b.id) toolIndex.set(b.id, t);
          }
        }
        if (userImgs && !imgsAttached) {
          items.push({ key: key(), kind: 'user', text: '', images: userImgs });
        }
      }
      // A tool card still running after a full replay means the run was
      // interrupted before the result landed — don't spin forever.
      for (const t of toolIndex.values()) {
        if (t.state === 'running') {
          t.state = 'error';
          t.result = i18nT('store.toolAborted');
        }
      }
      resetCtx(ctxFor(k));
      set(s => ({ bySession: { ...s.bySession, [k]: items } }));
    },
  };
});
