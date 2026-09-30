// Project-folder helpers for the history sidebar. Session headers record
// the cwd a session belongs to; the desktop groups history by that folder.

// normalizeFolder folds separator style and trailing separators so a
// header cwd (E:\a\b) compares equal to the workspace path the main
// process reports. Windows paths are case-insensitive; lower-casing is
// safe there and harmless in practice elsewhere.
export function normalizeFolder(p: string): string {
  return p.replace(/\//g, '\\').replace(/\\+$/, '').toLowerCase();
}

export function sameFolder(a: string | null | undefined, b: string | null | undefined): boolean {
  if (!a || !b) return false;
  return normalizeFolder(a) === normalizeFolder(b);
}

// folderName is the last path segment — the label shown for a group.
export function folderName(p: string): string {
  const parts = p.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] || p;
}
