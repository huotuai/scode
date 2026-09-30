import { useEffect, useRef, useState } from 'react';
import { ArrowLeft, ArrowRight, RotateCw, ExternalLink, X, Globe } from 'lucide-react';
import { useUiStore } from '../../store/ui';
import { openExternal } from '../../lib/rpc';
import type { WebviewElement } from '../../webview';
import '../../styles/browser.css';

// Right-side in-app browser. Session links open in this panel's <webview>
// (a separate guest process), so following a link can never navigate the
// chat UI itself away. The guest shares no session storage with the chat
// renderer (persist:browser partition keeps logins across panel opens).
export default function BrowserPanel() {
  const url = useUiStore(s => s.browserUrl);
  const width = useUiStore(s => s.browserWidth);
  const setWidth = useUiStore(s => s.setBrowserWidth);
  const close = useUiStore(s => s.closeBrowser);

  const wvRef = useRef<HTMLElement | null>(null);
  const [address, setAddress] = useState('');
  const [pageTitle, setPageTitle] = useState('');
  const [canBack, setCanBack] = useState(false);
  const [canFwd, setCanFwd] = useState(false);
  const [loading, setLoading] = useState(false);

  const wv = () => wvRef.current as WebviewElement | null;

  // Sync the address bar and nav buttons with the guest's state.
  useEffect(() => {
    const el = wv();
    if (!el || !url) return;
    const syncNav = (target?: string) => {
      setAddress(target || el.getURL() || url);
      setCanBack(el.canGoBack());
      setCanFwd(el.canGoForward());
    };
    const onNav = (e: Event) => syncNav((e as { url?: string }).url);
    const onTitle = (e: Event) => setPageTitle((e as { title?: string }).title || '');
    const onStart = () => setLoading(true);
    const onStop = () => {
      setLoading(false);
      syncNav();
    };
    el.addEventListener('did-navigate', onNav);
    el.addEventListener('did-navigate-in-page', onNav);
    el.addEventListener('page-title-updated', onTitle);
    el.addEventListener('did-start-loading', onStart);
    el.addEventListener('did-stop-loading', onStop);
    setAddress(url);
    return () => {
      el.removeEventListener('did-navigate', onNav);
      el.removeEventListener('did-navigate-in-page', onNav);
      el.removeEventListener('page-title-updated', onTitle);
      el.removeEventListener('did-start-loading', onStart);
      el.removeEventListener('did-stop-loading', onStop);
    };
  }, [url]);

  // Panel closed: render nothing (the guest process is torn down with it).
  if (!url) return null;

  const navigate = (raw: string) => {
    const target = /^https?:\/\//i.test(raw) ? raw : `https://${raw}`;
    wv()?.loadURL(target).catch(() => {});
    setAddress(target);
  };

  const onResizeStart = (e: React.PointerEvent) => {
    e.preventDefault();
    const startX = e.clientX;
    const startW = width;
    const onMove = (ev: PointerEvent) => setWidth(startW + (startX - ev.clientX));
    const onUp = () => {
      window.removeEventListener('pointermove', onMove);
      window.removeEventListener('pointerup', onUp);
    };
    window.addEventListener('pointermove', onMove);
    window.addEventListener('pointerup', onUp);
  };

  return (
    <aside className="browser-panel" style={{ width }}>
      <div className="browser-resize" onPointerDown={onResizeStart} />
      <div className="browser-bar">
        <button
          className="icon-btn"
          title="后退"
          disabled={!canBack}
          onClick={() => wv()?.goBack()}
        >
          <ArrowLeft size={15} />
        </button>
        <button
          className="icon-btn"
          title="前进"
          disabled={!canFwd}
          onClick={() => wv()?.goForward()}
        >
          <ArrowRight size={15} />
        </button>
        <button className="icon-btn" title="刷新" onClick={() => wv()?.reload()}>
          <RotateCw size={14} className={loading ? 'spin' : undefined} />
        </button>
        <form
          className="browser-address"
          onSubmit={e => {
            e.preventDefault();
            if (address.trim()) navigate(address.trim());
          }}
        >
          <Globe size={13} />
          <input
            value={address}
            onChange={e => setAddress(e.target.value)}
            onFocus={e => e.target.select()}
            spellCheck={false}
            aria-label="地址"
          />
        </form>
        <button
          className="icon-btn"
          title="在系统浏览器中打开"
          onClick={() => openExternal(wv()?.getURL() || url).catch(() => {})}
        >
          <ExternalLink size={14} />
        </button>
        <button className="icon-btn" title="关闭" onClick={close}>
          <X size={15} />
        </button>
      </div>
      {pageTitle && <div className="browser-title">{pageTitle}</div>}
      <webview ref={wvRef} className="browser-view" src={url} partition="persist:browser" />
    </aside>
  );
}
