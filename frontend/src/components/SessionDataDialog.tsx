import { useEffect, useRef, useState } from 'react';
import { Cookie, X } from 'lucide-react';

import { applySeed, buildSeedPayload, seedErrorText, type SeedResult } from '../lib/seedApi';

/**
 * Live custom-data dialog for a running session.
 *
 * One text box on purpose: the common case is pasting `sid=abc; theme=dark`
 * straight out of devtools, and the escape hatch is a full JSON /seed body
 * (cookies, headers, storage) when the caller needs the rest. The reload
 * checkbox is opt-in because it discards in-page state.
 */
export function SessionDataDialog({
  sessionId,
  token,
  onClose,
}: {
  sessionId: string;
  token: string;
  onClose: () => void;
}) {
  const [text, setText] = useState('');
  const [reload, setReload] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState<SeedResult | null>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  // Guards against a double-click firing two /seed calls before the disabled
  // state lands (state is async; the ref is not).
  const inFlight = useRef(false);
  // A JSON payload owns its own `reload` field (buildSeedPayload keeps it), so
  // the checkbox cannot apply to that form.
  const isJson = text.trim().startsWith('{') || text.trim().startsWith('[');

  // Focus once on mount. This must not depend on `onClose`: the parent passes
  // an inline arrow, and re-running it on every (≥1 Hz) parent render would
  // yank focus off the reload checkbox or Apply button.
  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose, busy]);

  const submit = async () => {
    if (inFlight.current) return;
    setError('');
    setResult(null);
    let payload;
    try {
      payload = buildSeedPayload(text, isJson ? false : reload);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Invalid input');
      return;
    }
    inFlight.current = true;
    setBusy(true);
    try {
      const res = await applySeed(sessionId, payload, token);
      setResult(res);
    } catch (err) {
      setError(seedErrorText(err));
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  };

  // Closing mid-apply would abandon the request with no result surfaced (the
  // seed may already have partially applied), so make Apply the only way out
  // until it settles.
  const close = () => {
    if (!busy) onClose();
  };

  return (
    <div className="fixed inset-0 z-[60] flex items-center justify-center">
      <div
        className="absolute inset-0 bg-black/40 backdrop-blur-sm"
        onClick={close}
        onContextMenu={(e) => { e.preventDefault(); close(); }}
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="session-data-title"
        className="relative w-[min(560px,92vw)] rounded-xl border border-zinc-200 dark:border-gray-700 bg-white dark:bg-gray-900 shadow-2xl"
      >
        <div className="flex items-center gap-2 border-b border-zinc-200 dark:border-gray-700 px-4 py-3">
          <Cookie size={16} className="text-cyan-600 dark:text-cyan-400" />
          <h2 id="session-data-title" className="flex-1 text-sm font-medium text-zinc-900 dark:text-white">
            Custom data for this page
          </h2>
          <button
            type="button"
            aria-label="Close"
            onClick={close}
            className="rounded-md p-1 text-zinc-500 hover:bg-zinc-100 hover:text-zinc-900 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-white"
          >
            <X size={16} />
          </button>
        </div>

        <div className="space-y-3 px-4 py-3">
          <label className="block text-xs text-zinc-600 dark:text-gray-400" htmlFor="session-data-input">
            Cookies (<code className="font-mono">name=value; …</code>) or a JSON payload with
            <code className="ml-1 font-mono">cookies</code>, <code className="font-mono">headers</code> and
            <code className="ml-1 font-mono">storage</code>. Cookies without a domain apply to this session's site.
          </label>
          <textarea
            id="session-data-input"
            ref={inputRef}
            value={text}
            onChange={(e) => { setText(e.target.value); setError(''); setResult(null); }}
            spellCheck={false}
            rows={6}
            placeholder={'sid=abc123; theme=dark\n\nor\n\n{"headers":[{"name":"X-Api-Key","value":"…"}]}'}
            className="w-full resize-y rounded-lg border border-zinc-300 dark:border-gray-700 bg-zinc-50 dark:bg-gray-950 p-2 font-mono text-xs text-zinc-800 dark:text-gray-200 outline-none focus:border-cyan-500"
          />

          <label className="flex items-center gap-2 text-xs text-zinc-700 dark:text-gray-300">
            <input
              type="checkbox"
              checked={reload && !isJson}
              disabled={isJson}
              onChange={(e) => setReload(e.target.checked)}
              className="h-3.5 w-3.5 accent-cyan-600 disabled:opacity-50"
            />
            Reload the page after applying (discards in-page state){isJson ? ' — JSON carries its own reload' : ''}
          </label>

          {error && (
            <p role="alert" className="rounded-md bg-red-500/10 px-2 py-1.5 text-xs text-red-600 dark:text-red-400">
              {error}
            </p>
          )}
          {result && !error && (
            <p role="status" className="rounded-md bg-emerald-500/10 px-2 py-1.5 text-xs text-emerald-700 dark:text-emerald-400">
              Applied {result.cookies} cookie{result.cookies === 1 ? '' : 's'}, {result.headers} header
              {result.headers === 1 ? '' : 's'}, {result.storage} storage item
              {result.storage === 1 ? '' : 's'}
              {result.reloaded ? ' — page reloaded' : ''}.
            </p>
          )}
        </div>

        <div className="flex justify-end gap-2 border-t border-zinc-200 dark:border-gray-700 px-4 py-3">
          <button
            type="button"
            onClick={close}
            className="rounded-lg px-3 py-1.5 text-xs text-zinc-600 hover:bg-zinc-100 dark:text-gray-300 dark:hover:bg-gray-800"
          >
            Close
          </button>
          <button
            type="button"
            onClick={() => void submit()}
            disabled={busy || !text.trim()}
            className="rounded-lg bg-cyan-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-cyan-500 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {busy ? 'Applying…' : 'Apply'}
          </button>
        </div>
      </div>
    </div>
  );
}
