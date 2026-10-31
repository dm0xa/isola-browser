import { useEffect, useRef, useState } from 'react';
import { Clock, Check } from 'lucide-react';

export function formatTime(totalSec: number): string {
  if (totalSec <= 0) return '0:00';
  const m = Math.floor(totalSec / 60);
  const s = totalSec % 60;
  return `${m}:${s.toString().padStart(2, '0')}`;
}

type Urgency = 'calm' | 'warning' | 'critical';

/**
 * Session clock + extend action as a single control.
 *
 * The remaining time and the "add 5 minutes" action govern the same thing, so
 * they read as one unit instead of a passive countdown in one place and a
 * detached link in another. Urgency escalates the readout (neutral → amber →
 * red) and promotes the extend action so the owner can act before the session
 * is torn down.
 */
export default function SessionTimeControl({
  remainingSec,
  isUnlimited,
  status,
  extendAvailable,
  viewOnly,
  onExtend,
}: {
  remainingSec: number;
  isUnlimited: boolean;
  status: string;
  extendAvailable: boolean;
  viewOnly: boolean;
  onExtend: () => Promise<boolean> | boolean;
}) {
  const [added, setAdded] = useState(false);
  const addedTimer = useRef<number | null>(null);

  useEffect(() => () => {
    if (addedTimer.current !== null) window.clearTimeout(addedTimer.current);
  }, []);

  // Nothing to show (and nothing to control) once the session has failed.
  if (status === 'error') return null;

  // The countdown is only meaningful for a live, limited session. Before the
  // session starts we show a placeholder rather than a red "0:00".
  const notStarted =
    remainingSec <= 0 && (status === 'provisioning' || status === 'starting');
  const live = !isUnlimited && !notStarted;
  const urgency: Urgency = !live
    ? 'calm'
    : remainingSec <= 60
    ? 'critical'
    : remainingSec <= 180
    ? 'warning'
    : 'calm';

  const timeText = isUnlimited
    ? 'unlimited'
    : notStarted
    ? '…'
    : formatTime(remainingSec);

  const canExtend = !viewOnly && extendAvailable && !isUnlimited && status !== 'error';
  const disabled = remainingSec <= 0;

  const readoutColor = isUnlimited
    ? 'text-zinc-500 dark:text-gray-400'
    : urgency === 'critical'
    ? 'text-red-600 dark:text-red-400'
    : urgency === 'warning'
    ? 'text-amber-600 dark:text-amber-400'
    : 'text-zinc-600 dark:text-gray-300';

  const iconColor =
    urgency === 'critical'
      ? 'text-red-500 dark:text-red-400'
      : urgency === 'warning'
      ? 'text-amber-500 dark:text-amber-400'
      : 'text-zinc-400 dark:text-gray-500';

  const actionTone = added
    ? 'text-emerald-700 dark:text-emerald-300'
    : urgency === 'critical'
    ? 'text-red-700 hover:bg-red-500/15 dark:text-red-200 dark:hover:bg-red-400/15'
    : urgency === 'warning'
    ? 'text-amber-800 hover:bg-amber-500/15 dark:text-amber-200 dark:hover:bg-amber-400/15'
    : 'text-zinc-600 hover:bg-zinc-300/80 dark:text-gray-300 dark:hover:bg-gray-700';

  const handleExtend = async () => {
    const ok = await onExtend();
    if (ok === false) return;
    setAdded(true);
    if (addedTimer.current !== null) window.clearTimeout(addedTimer.current);
    addedTimer.current = window.setTimeout(() => setAdded(false), 1800);
  };

  return (
    <div
      className={`inline-flex h-7 items-center text-xs ${
        canExtend
          ? 'rounded-md bg-zinc-200/80 ring-1 ring-inset ring-zinc-300/80 dark:bg-gray-800 dark:ring-gray-700'
          : ''
      }`}
    >
      <span className="flex items-center gap-1.5 px-2 font-mono tabular-nums">
        <Clock
          size={13}
          aria-hidden
          className={`${iconColor} ${urgency === 'critical' ? 'motion-safe:animate-pulse' : ''}`}
        />
        <span className={readoutColor}>{timeText}</span>
      </span>

      {canExtend && (
        <>
          <span aria-hidden className="h-3.5 w-px shrink-0 bg-zinc-300 dark:bg-gray-600" />
          <button
            type="button"
            onClick={() => void handleExtend()}
            disabled={disabled}
            title="Extend session by 5 minutes"
            aria-label="Extend session by 5 minutes"
            className={`flex h-full items-center gap-1 rounded-r-[5px] px-2 font-mono tabular-nums transition-colors focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-zinc-400/70 dark:focus-visible:ring-gray-500 disabled:cursor-not-allowed disabled:opacity-40 ${actionTone}`}
          >
            {added ? (
              <>
                <Check size={12} aria-hidden strokeWidth={2.5} />
                <span>ok</span>
              </>
            ) : (
              <span>+5m</span>
            )}
          </button>
        </>
      )}
    </div>
  );
}
