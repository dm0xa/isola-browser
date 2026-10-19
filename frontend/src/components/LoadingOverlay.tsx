import type { ReactNode, ReactElement, CSSProperties } from 'react';
import { cloneElement, isValidElement } from 'react';
import { ISOLA_LOGO_KEYFRAMES, IsolaLogo } from './IsolaLogo';

type Phase = 'provisioning' | 'launching' | 'connecting' | 'ready' | 'restarting';

function cloneWithClass(n: ReactNode, className: string): ReactNode {
  if (!isValidElement(n)) return n;
  const el = n as ReactElement<any>;
  return cloneElement(el, {
    className: el.props.className ? `${el.props.className} ${className}` : className,
  });
}

function cloneWithStrokeStyle(n: ReactNode, className: string, animation: string): ReactNode {
  if (!isValidElement(n)) return n;
  const el = n as ReactElement<any>;
  const existing = (el.props.style || {}) as CSSProperties;
  return cloneElement(el, {
    className: el.props.className ? `${el.props.className} ${className}` : className,
    strokeWidth: el.props.strokeWidth ?? 10,
    strokeLinejoin: el.props.strokeLinejoin ?? 'miter',
    style: { ...existing, animation },
    fill: el.props.fill ?? 'none',
  });
}

type LoadingOverlayProps = {
  status: string;
  isReconnecting: boolean;
  isRestarting?: boolean;
  caption?: string;
  /**
   * - 'launch' (default): full 4-phase flow (Provisioning → Launching → Connecting → Ready).
   *     Used by the app-level launch overlay (App.tsx) while a new session is
   *     spun up from scratch, and by a directly opened ViewerPage.
   * - 'connect': reduced 2-phase flow (Connecting → Ready). Used on SharePage
   *     join flows where the container already exists and the viewer only
   *     needs to establish the stream.
   */
  phases?: 'launch' | 'connect';
};

const PHASES_LAUNCH: { key: Phase; label: string }[] = [
  { key: 'provisioning', label: 'Provisioning container' },
  { key: 'launching', label: 'Launching browser' },
  { key: 'connecting', label: 'Establishing stream' },
  { key: 'ready', label: 'Ready' },
];

const PHASES_CONNECT: { key: Phase; label: string }[] = [
  { key: 'connecting', label: 'Establishing stream' },
  { key: 'ready', label: 'Ready' },
];

function resolvePhase(status: string, isReconnecting: boolean, isRestarting?: boolean): Phase {
  if (isRestarting) return 'restarting';
  if (isReconnecting) return 'connecting';
  switch (status) {
    case 'provisioning':
      return 'provisioning';
    case 'starting':
      return 'launching';
    case 'running':
      return 'connecting';
    case 'ready':
      return 'ready';
    default:
      return 'provisioning';
  }
}

export function LoadingOverlay({ status, isReconnecting, isRestarting, caption, phases }: LoadingOverlayProps) {
  const phaseList = phases === 'connect' ? PHASES_CONNECT : PHASES_LAUNCH;
  const activePhase = resolvePhase(status, isReconnecting, isRestarting);
  const activeIndex = phaseList.findIndex((p) => p.key === activePhase);
  // If activeIndex is -1 (e.g. 'provisioning' on the connect-only list, which
  // only exposes connecting+ready), clamp to the FIRST phase so the dot shows
  // progress instead of a list of all-pending checkmarks.
  const clampedIndex = activeIndex >= 0 ? activeIndex : 0;

  return (
    <div className="loader-root absolute inset-0 z-40 flex flex-col items-center justify-center">
      {/* Logo */}
      <div className="relative mb-10">
        <IsolaLogo
          className="loader-logo w-24 h-24"
          float
          glow={false}
          renderFaces={(faces) => {
            const children = Array.isArray(faces) ? (faces as ReactNode[]) : null;
            if (!children || children.length < 3) return null as unknown as ReactNode;
            // Inject per-path loader classNames: face, face-left, face-right
            const [face, faceL, faceR] = children;
            return (
              <>
                {cloneWithClass(face, 'loader-logo-face')}
                {cloneWithClass(faceL, 'loader-logo-face-left')}
                {cloneWithClass(faceR, 'loader-logo-face-right')}
              </>
            ) as unknown as ReactNode;
          }}
          renderRing={(ring) => {
            const children = Array.isArray(ring) ? (ring as ReactNode[]) : null;
            if (!children || children.length < 3) return null as unknown as ReactNode;
            const delays = ['', ' 0.3s', ' 0.6s'];
            const cls = 'loader-logo-stroke';
            return (
              <>
                {children.map((c, i) => cloneWithStrokeStyle(c, cls, `loader-stroke-trace 2.4s ease-in-out infinite${delays[i]}`))}
              </>
            ) as unknown as ReactNode;
          }}
          renderTop={(top) => {
            const children = Array.isArray(top) ? (top as ReactNode[]) : null;
            if (!children || children.length < 3) return null as unknown as ReactNode;
            const [topMain, topL, topR] = children;
            return (
              <>
                {cloneWithClass(topMain, 'loader-logo-top')}
                {cloneWithClass(topL, 'loader-logo-top-left')}
                {cloneWithClass(topR, 'loader-logo-top-right')}
              </>
            ) as unknown as ReactNode;
          }}
        />
      </div>

      {/* Phase progress — during restart we collapse to a single
          "Restarting session" line so the user doesn't see stale phases
          from the previous run. */}
      <div className="w-80 max-w-[80vw] flex flex-col gap-2">
        {isRestarting ? (
          <div className="flex items-center gap-3 text-xs font-mono">
            <span className="w-4 flex justify-center">
              <span
                className="loader-phase-dot inline-block w-1.5 h-1.5 rounded-full"
                style={{ animation: 'loader-dot 1.2s ease-in-out infinite' }}
              />
            </span>
            <span className="loader-phase-active">Restarting session</span>
            <span className="loader-phase-progress ml-auto animate-pulse">…</span>
          </div>
        ) : (
          phaseList.map((phase, index) => {
            const state =
              index < clampedIndex ? 'done' : index === clampedIndex ? 'active' : 'pending';
            return (
              <div key={phase.key} className="flex items-center gap-3 text-xs font-mono">
                <span className="w-4 flex justify-center">
                  {state === 'done' ? (
                    <span className="loader-phase-done" aria-hidden="true">✓</span>
                  ) : state === 'active' ? (
                    <span
                      className="loader-phase-dot inline-block w-1.5 h-1.5 rounded-full"
                      style={{ animation: 'loader-dot 1.2s ease-in-out infinite' }}
                    />
                  ) : (
                    <span className="loader-phase-pending" aria-hidden="true">○</span>
                  )}
                </span>
                <span
                  className={
                    state === 'pending'
                      ? 'loader-phase-pending'
                      : state === 'active'
                      ? 'loader-phase-active'
                      : 'loader-phase-done-text'
                  }
                >
                  {phase.label}
                </span>
                {state === 'active' && (
                  <span className="loader-phase-progress ml-auto animate-pulse">…</span>
                )}
              </div>
            );
          })
        )}
      </div>

      {/* Bottom hint */}
      <p className="absolute bottom-6 text-[11px] font-mono loader-bottom-hint tracking-wider">
        {caption ??
          (isRestarting ? 'RESTARTING SESSION' : isReconnecting ? 'RECONNECTING TO SESSION' : 'STARTING REMOTE BROWSER')}
      </p>

      <style>{ISOLA_LOGO_KEYFRAMES + `
        @keyframes loader-logo-pulse {
          0%, 100% { filter: drop-shadow(0 0 12px var(--loader-glow)); transform: translateY(0); }
          50% { filter: drop-shadow(0 0 28px var(--loader-glow-strong)); transform: translateY(-3px); }
        }
        @keyframes loader-stroke-trace {
          0%, 100% { opacity: 0.35; }
          50% { opacity: 1; }
        }
        @keyframes loader-cursor-blink {
          0%, 49% { opacity: 1; }
          50%, 100% { opacity: 0; }
        }
        @keyframes loader-dot {
          0%, 100% { transform: scale(1); opacity: 0.5; }
          50% { transform: scale(1.4); opacity: 1; }
        }

        .loader-logo { animation: loader-logo-pulse 2.4s ease-in-out infinite; }

        /* === Dark theme === */
        .dark .loader-root {
          background: #030712;
          background-image: radial-gradient(circle at 50% 40%, rgba(0,229,255,0.06) 0%, rgba(3,7,18,0) 60%);
          color: #aea2a2;
          --loader-accent: #00e5ff;
          --loader-accent-soft: rgba(0, 229, 255, 0.7);
          --loader-glow: rgba(0,229,255,0.25);
          --loader-glow-strong: rgba(0,229,255,0.6);
        }
        .dark .loader-logo-face { fill: #1a2024; }
        .dark .loader-logo-face-left { fill: #0f1418; }
        .dark .loader-logo-face-right { fill: #0a0e10; }
        .dark .loader-logo-stroke { stroke: #00e5ff; }
        .dark .loader-logo-top { fill: #ffffff; }
        .dark .loader-logo-top-left { fill: #e0e0e0; }
        .dark .loader-logo-top-right { fill: #cccccc; }
        .dark .loader-term-prompt { color: #00e5ff; }
        .dark .loader-term-cmd { color: #ffffff; }
        .dark .loader-term-flag { color: #6b7280; }
        .dark .loader-term-value { color: #d1d5db; }
        .dark .loader-cursor { background: #00e5ff; }
        .dark .loader-phase-done { color: #00e5ff; }
        .dark .loader-phase-dot { background: #00e5ff; box-shadow: 0 0 8px rgba(0,229,255,0.7); }
        .dark .loader-phase-pending { color: #374151; }
        .dark .loader-phase-active { color: #67e8f9; }
        .dark .loader-phase-done-text { color: #9ca3af; }
        .dark .loader-phase-progress { color: #6b7280; }
        .dark .loader-bottom-hint { color: #4b5563; }

        /* === Light theme === */
        :root:not(.dark) .loader-root {
          background: #f4f4f5;
          background-image: radial-gradient(circle at 50% 40%, rgba(8,145,178,0.08) 0%, rgba(244,244,245,0) 60%);
          color: #3f3f46;
          --loader-accent: #0891b2;
          --loader-accent-soft: rgba(8, 145, 178, 0.7);
          --loader-glow: rgba(8, 145, 178, 0.18);
          --loader-glow-strong: rgba(8, 145, 178, 0.35);
        }
        :root:not(.dark) .loader-logo-face { fill: #e4e4e7; }
        :root:not(.dark) .loader-logo-face-left { fill: #d4d4d8; }
        :root:not(.dark) .loader-logo-face-right { fill: #c8c8cc; }
        :root:not(.dark) .loader-logo-stroke { stroke: #0891b2; }
        :root:not(.dark) .loader-logo-top { fill: #18181b; }
        :root:not(.dark) .loader-logo-top-left { fill: #27272a; }
        :root:not(.dark) .loader-logo-top-right { fill: #3f3f46; }
        :root:not(.dark) .loader-term-prompt { color: #0891b2; }
        :root:not(.dark) .loader-term-cmd { color: #18181b; }
        :root:not(.dark) .loader-term-flag { color: #71717a; }
        :root:not(.dark) .loader-term-value { color: #27272a; }
        :root:not(.dark) .loader-cursor { background: #0891b2; }
        :root:not(.dark) .loader-phase-done { color: #0891b2; }
        :root:not(.dark) .loader-phase-dot { background: #0891b2; box-shadow: 0 0 6px rgba(8,145,178,0.5); }
        :root:not(.dark) .loader-phase-pending { color: #d4d4d8; }
        :root:not(.dark) .loader-phase-active { color: #0e7490; }
        :root:not(.dark) .loader-phase-done-text { color: #52525b; }
        :root:not(.dark) .loader-phase-progress { color: #71717a; }
        :root:not(.dark) .loader-bottom-hint { color: #a1a1aa; }
      `}</style>
    </div>
  );
}
