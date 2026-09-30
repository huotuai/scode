import { useRef, useState } from 'react';
import { ArrowUp, Square, X } from 'lucide-react';
import { useSessionStore, useActiveRuntime } from '../../store/session';
import { useChatStore, DRAFT_KEY } from '../../store/chat';
import ModelPicker from './ModelPicker';
import SandboxPicker from './SandboxPicker';
import PlusMenu from './PlusMenu';
import { useT } from '../../i18n';
import '../../styles/composer.css';

// One staged attachment: the data URL serves both as the thumbnail src
// and the wire payload (the store strips the prefix before sending).
interface PendingImage {
  id: number;
  dataUrl: string;
}

// Mirrors the backend caps (internal/server maxPromptImages, cli
// maxImageBytes): oversize or excess attachments would be silently
// dropped server-side, so reject them here with a visible note.
const MAX_IMAGES = 8;
const MAX_IMAGE_BYTES = 10 * 1024 * 1024;

function readAsDataUrl(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(String(r.result));
    r.onerror = () => reject(r.error);
    r.readAsDataURL(file);
  });
}

export default function Composer() {
  const t = useT();
  const [text, setText] = useState('');
  const [images, setImages] = useState<PendingImage[]>([]);
  // Mirror of `images` for async addFiles loops (state closures go stale
  // while awaiting FileReader for a multi-file paste).
  const imagesRef = useRef<PendingImage[]>([]);
  const nextImgId = useRef(1);
  const send = useSessionStore(s => s.send);
  const cancel = useSessionStore(s => s.cancel);
  const rt = useActiveRuntime();
  const running = rt.running;
  const compacting = rt.compacting;
  const sessionId = useSessionStore(s => s.sessionId);

  const canSend = text.trim().length > 0 || images.length > 0;

  // Staging problems (oversize, too many) surface as a note in the chat
  // log — the composer has no inline error surface of its own.
  const note = (msg: string) =>
    useChatStore.getState().addNote(sessionId ?? DRAFT_KEY, msg);

  const setImgs = (updater: (cur: PendingImage[]) => PendingImage[]) => {
    setImages(cur => {
      const next = updater(cur);
      imagesRef.current = next;
      return next;
    });
  };

  const addFiles = async (files: File[]) => {
    for (const f of files) {
      if (!f.type.startsWith('image/')) continue;
      if (imagesRef.current.length >= MAX_IMAGES) {
        note(t('composer.tooManyImages', { max: MAX_IMAGES }));
        break;
      }
      if (f.size > MAX_IMAGE_BYTES) {
        note(t('composer.imageTooBig', { mb: Math.round(MAX_IMAGE_BYTES / 1024 / 1024), name: f.name || t('composer.clipboardImage') }));
        continue;
      }
      try {
        const dataUrl = await readAsDataUrl(f);
        setImgs(cur =>
          cur.length >= MAX_IMAGES ? cur : [...cur, { id: nextImgId.current++, dataUrl }],
        );
      } catch {
        note(t('composer.imageReadFail'));
      }
    }
  };

  const onPaste = (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
    // Clipboard screenshots and copied image files both arrive as
    // file-kind items. A pure-text paste falls through untouched.
    const files: File[] = [];
    for (const item of Array.from(e.clipboardData.items)) {
      if (item.kind === 'file' && item.type.startsWith('image/')) {
        const f = item.getAsFile();
        if (f) files.push(f);
      }
    }
    if (files.length === 0) return;
    e.preventDefault();
    addFiles(files);
  };

  const doSend = async () => {
    if (!canSend) return;
    const value = text;
    const imgs = images;
    setText('');
    setImgs(() => []);
    // send() returns false when the draft was never consumed (the session
    // failed to start); restore it so the user can retry.
    const ok = await send(value, imgs.map(i => i.dataUrl));
    if (!ok) {
      setText(cur => (cur ? cur : value));
      setImgs(cur => (cur.length > 0 ? cur : imgs));
    }
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      doSend();
    }
  };

  return (
    <div className="composer-wrap">
      <div className="composer">
        {images.length > 0 && (
          <div className="composer-images">
            {images.map(img => (
              <div className="composer-image" key={img.id}>
                <img src={img.dataUrl} alt={t('composer.imageAlt')} />
                <button
                  className="composer-image-remove"
                  onClick={() => setImgs(cur => cur.filter(i => i.id !== img.id))}
                  title={t('composer.removeImage')}
                >
                  <X size={11} />
                </button>
              </div>
            ))}
          </div>
        )}
        <textarea
          value={text}
          onChange={e => setText(e.target.value)}
          onKeyDown={onKeyDown}
          onPaste={onPaste}
          rows={2}
          placeholder={
            running ? t('composer.placeholderRunning') : t('composer.placeholderIdle')
          }
        />
        <div className="composer-bar">
          <PlusMenu onFiles={addFiles} />
          <ModelPicker />
          <SandboxPicker />
          <span className="spacer" />
          {running && (
            <button className="icon-btn stop-btn" onClick={cancel} title={t('composer.stop')}>
              <Square size={13} fill="currentColor" />
            </button>
          )}
          <button
            className="send-btn"
            onClick={doSend}
            disabled={!canSend || compacting}
            title={compacting ? t('composer.compactingTitle') : running ? t('composer.steer') : t('composer.send')}
          >
            <ArrowUp size={16} />
          </button>
        </div>
      </div>
    </div>
  );
}
