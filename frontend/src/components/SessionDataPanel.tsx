import { useEffect, useRef, useState } from 'react';
import { Cookie, X } from 'lucide-react';

import { applySeed, buildSeedPayload, seedErrorText, type SeedResult } from '../lib/seedApi';

/** Float inset, and the extra top offset that clears the 36px isola tab strip. */
const FLOAT_INSET = 12;

// Slide in from the edge: the panel is a dock, not a popup, so it enters like
// the other viewerside overlays instead of appearing on top of the stream.
const PANEL_MOTION = `
@keyframes isola-session-data-in {
  from { opacity: 0; transform: translateX(14px); }
  to { opacity: 1; transform: none; }
}
.isola-session-data-panel { animation: isola-session-data-in 150ms ease-out; }
@media (prefers-reduced-motion: reduce) {
  .isola-session-data-panel { animation: none; }
}
`;

/**
 * Live custom-data panel docked to the right of the stream.
 *
 * One text box on purpose: the common case is pasting `sid=abc; theme=dark`
 * straight out of devtools, and the escape hatch is a full JSON /seed body
 * (cookies, headers, storage) when the caller needs the rest. The reload
 * checkbox is opt-in because it discards in-page state.
 *
 * Non-modal: unlike a dialog there is no backdrop, so the operator can keep
 * reading (and clicking) the live page while composing a payload. Escape still
 * closes it, and it unmounts with the viewer.
 */
export function SessionDataPanel({
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
    <>
      <style>{PANEL_MOTION}</style>
      <aside
        role="dialog"
        aria-labelledby="session-data-title"
        className="isola-session-data-panel absolute z-40 flex flex-col overflow-hidden rounded-xl border border-cyan-500/30 bg-white text-xs text-zinc-800 shadow-2xl dark:border-cyan-500/20 dark:bg-slate-950 dark:text-gray-200"
        style={{
          top: FLOAT_INSET + 32,
          right: FLOAT_INSET,
          bottom: FLOAT_INSET,
          width: 'min(420px, calc(100% - 24px))',
        }}
      >
        <div className="flex shrink-0 items-center gap-2 border-b border-zinc-200 px-3 py-2 dark:border-gray-700">
          <Cookie size={15} className="text-cyan-600 dark:text-cyan-400" />
          <h2 id="session-data-title" className="flex-1 text-sm font-medium text-zinc-900 dark:text-white">
            Custom data for this page
          </h2>
          <button
            type="button"
            aria-label="Close"
            onClick={close}
            disabled={busy}
            className="rounded-md p-1 text-zinc-500 hover:bg-zinc-100 hover:text-zinc-900 disabled:cursor-not-allowed disabled:opacity-50 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-white"
          >
            <X size={16} />
          </button>
        </div>

        <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto px-3 py-3">
          <label className="block text-xs leading-relaxed text-zinc-600 dark:text-gray-400" htmlFor="session-data-input">
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
            placeholder={'sid=abc123; theme=dark\n\nor\n\n{"headers":[{"name":"X-Api-Key","value":"…"}]}'}
            className="min-h-[140px] flex-1 resize-none rounded-lg border border-zinc-300 bg-zinc-50 p-2 font-mono text-xs text-zinc-800 outline-none focus:border-cyan-500 dark:border-gray-700 dark:bg-gray-950 dark:text-gray-200"
          />

          <label className="flex shrink-0 items-start gap-2 text-xs text-zinc-700 dark:text-gray-300">
            <input
              type="checkbox"
              checked={reload && !isJson}
              disabled={isJson}
              onChange={(e) => setReload(e.target.checked)}
              className="mt-0.5 h-3.5 w-3.5 shrink-0 accent-cyan-600 disabled:opacity-50"
            />
            <span>
              Reload the page after applying (discards in-page state)
              {isJson ? ' — JSON carries its own reload' : ''}
            </span>
          </label>

          {error && (
            <p role="alert" className="shrink-0 rounded-md bg-red-500/10 px-2 py-1.5 text-xs text-red-600 dark:text-red-400">
              {error}
            </p>
          )}
          {result && !error && (
            <p role="status" className="shrink-0 rounded-md bg-emerald-500/10 px-2 py-1.5 text-xs text-emerald-700 dark:text-emerald-400">
              Applied {result.cookies} cookie{result.cookies === 1 ? '' : 's'}, {result.headers} header
              {result.headers === 1 ? '' : 's'}, {result.storage} storage item
              {result.storage === 1 ? '' : 's'}
              {result.reloaded ? ' — page reloaded' : ''}.
            </p>
          )}
        </div>

        <div className="flex shrink-0 justify-end gap-2 border-t border-zinc-200 px-3 py-2 dark:border-gray-700">
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
      </aside>
    </>
  );
}
