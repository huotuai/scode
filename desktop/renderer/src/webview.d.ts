// <webview> is an Electron built-in element, not part of React's DOM
// typings. Augment the JSX namespace so the browser panel can render it.
import 'react';

declare module 'react' {
  namespace JSX {
    interface IntrinsicElements {
      webview: React.DetailedHTMLProps<React.HTMLAttributes<HTMLElement>, HTMLElement> & {
        src?: string;
        partition?: string;
        allowpopups?: string;
      };
    }
  }
}

// The runtime API of a <webview> element (the subset the panel uses).
// Electron ships full typings but the renderer tsconfig does not pull in
// node/electron types, so a minimal local shape keeps typecheck honest.
export interface WebviewElement extends HTMLElement {
  loadURL(url: string): Promise<void>;
  goBack(): void;
  goForward(): void;
  reload(): void;
  canGoBack(): boolean;
  canGoForward(): boolean;
  getURL(): string;
  isLoading(): boolean;
}
