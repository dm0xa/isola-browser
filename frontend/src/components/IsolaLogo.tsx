import type { CSSProperties, ReactNode } from 'react';
import { useTheme } from '../lib/ThemeContext';

export const ISOLA_LOGO_KEYFRAMES = `
@keyframes logo-glow {
  0%, 100% { filter: drop-shadow(0 0 8px rgba(0,229,255,0.3)); }
  50% { filter: drop-shadow(0 0 18px rgba(0,229,255,0.6)); }
}
@keyframes logo-glow-light {
  0%, 100% { filter: drop-shadow(0 0 4px rgba(0,0,0,0.15)); }
  50% { filter: drop-shadow(0 0 10px rgba(0,0,0,0.25)); }
}
@keyframes logo-float-base {
  0% { transform: translate(0, 0); }
  25% { transform: translate(9px, 13px); }
  50% { transform: translate(13px, 5px); }
  75% { transform: translate(4px, -3px); }
  100% { transform: translate(0, 0); }
}
@keyframes logo-float-ring {
  0% { transform: translate(0, 0); }
  25% { transform: translate(-10px, -8px); }
  50% { transform: translate(-13px, 2px); }
  75% { transform: translate(-4px, 7px); }
  100% { transform: translate(0, 0); }
}
@keyframes logo-float-top {
  0% { transform: translate(0, 0); }
  30% { transform: translate(6px, -14px); }
  55% { transform: translate(-4px, -18px); }
  80% { transform: translate(2px, -7px); }
  100% { transform: translate(0, 0); }
}
.logo-float-base { animation: logo-float-base 6s ease-in-out infinite; }
.logo-float-ring { animation: logo-float-ring 8s ease-in-out infinite; }
.logo-float-top { animation: logo-float-top 7s ease-in-out infinite; }
/* Compact variant for the 36px chrome logo. The hero offsets above are in SVG
   user units, so at 36px they would be sub-pixel; these are the same drift
   scaled up to read at that size. */
@keyframes logo-mini-base {
  0% { transform: translate(0, 0); }
  25% { transform: translate(14px, 19px); }
  50% { transform: translate(22px, 8px); }
  75% { transform: translate(6px, -4px); }
  100% { transform: translate(0, 0); }
}
@keyframes logo-mini-ring {
  0% { transform: translate(0, 0); }
  25% { transform: translate(-17px, -13px); }
  50% { transform: translate(-24px, 4px); }
  75% { transform: translate(-7px, 12px); }
  100% { transform: translate(0, 0); }
}
@keyframes logo-mini-top {
  0% { transform: translate(0, 0); }
  30% { transform: translate(11px, -25px); }
  55% { transform: translate(-7px, -32px); }
  80% { transform: translate(4px, -12px); }
  100% { transform: translate(0, 0); }
}
.logo-mini-base { animation: logo-mini-base 6s ease-in-out infinite; }
.logo-mini-ring { animation: logo-mini-ring 8s ease-in-out infinite; }
.logo-mini-top { animation: logo-mini-top 7s ease-in-out infinite; }
`;

interface IsolaLogoPath {
  d: string;
  fill?: string;
  stroke?: string;
  strokeWidth?: number;
}

/** Static logo geometry (logo-local coords, centred on 0,0) — single source of
 * truth for the live component AND the PNG-export watermark stamp. */
export const ISOLA_LOGO_VIEWBOX = '0 0 512 512';
export const ISOLA_LOGO_CENTER = 'translate(256, 270)';
export const ISOLA_LOGO_FACE_PATHS: IsolaLogoPath[] = [
  { d: 'M 0 50 L 110 0 L 0 -50 L -110 0 Z', fill: '#1a2024' },
  { d: 'M -110 0 L 0 50 L 0 85 L -110 35 Z', fill: '#0f1418' },
  { d: 'M 110 0 L 0 50 L 0 85 L 110 35 Z', fill: '#0a0e10' },
];
export const ISOLA_LOGO_RING_PATHS: IsolaLogoPath[] = [
  { d: 'M 0 -20 L 100 -65 L 0 -110 L -100 -65 Z', stroke: '#00e5ff', strokeWidth: 10 },
  { d: 'M -100 -65 L 0 -20 L 0 5 L -100 -40 Z', stroke: '#00e5ff', strokeWidth: 10 },
  { d: 'M 100 -65 L 0 -20 L 0 5 L 100 -40 Z', stroke: '#00e5ff', strokeWidth: 10 },
];
export const ISOLA_LOGO_TOP_PATHS: IsolaLogoPath[] = [
  { d: 'M 0 -85 L 85 -125 L 0 -165 L -85 -125 Z', fill: '#ffffff' },
  { d: 'M -85 -125 L 0 -85 L 0 -65 L -85 -105 Z', fill: '#e0e0e0' },
  { d: 'M 85 -125 L 0 -85 L 0 -65 L 85 -105 Z', fill: '#cccccc' },
];

interface IsolaLogoProps {
  className?: string;
  style?: CSSProperties;
  /** Float animation (ring/top/base drift) — on Landing/Share pages */
  float?: boolean;
  /**
   * Compact layered drift for the 36px chrome logo (viewer/share toolbars).
   * The hero float is tuned for 96px and would be sub-pixel this small.
   */
  mini?: boolean;
  /** Neon glow pulse — on by default; set false for loaders with custom stroke animations */
  glow?: boolean;
  /** Passed through to the <svg> element — set to false to prevent HTML5 drag-ghost */
  draggable?: boolean;
  /** Override individual path renders for the loader (custom classNames / inline styles) */
  renderFaces?: (children: ReactNode) => ReactNode;
  renderRing?: (children: ReactNode) => ReactNode;
  renderTop?: (children: ReactNode) => ReactNode;
}

/**
 * Shared Isola diamond logo. Rendered inline (4 SVG path groups + keyframes).
 * Previously duplicated across LandingPage, ViewerPage, SharePage, and
 * LoadingOverlay; this component replaces all four copies with a single
 * source of truth with toggleable float/glow animations and per-layer
 * overrides for the loader's custom stroke-trace treatment.
 */
export function IsolaLogo({
  className = '',
  style,
  float = false,
  mini = false,
  glow = true,
  draggable,
  renderFaces,
  renderRing,
  renderTop,
}: IsolaLogoProps) {
  const { theme } = useTheme();

  const faces = ISOLA_LOGO_FACE_PATHS.map((p, i) => (
    <path key={`f${i}`} d={p.d} fill={p.fill} />
  ));
  const ring = ISOLA_LOGO_RING_PATHS.map((p, i) => (
    <path key={`r${i}`} d={p.d} fill="none" stroke={p.stroke} strokeWidth={p.strokeWidth} strokeLinejoin="miter" />
  ));
  const top = ISOLA_LOGO_TOP_PATHS.map((p, i) => (
    <path key={`t${i}`} d={p.d} fill={p.fill} />
  ));

  const mergedStyle: CSSProperties = {
    overflow: 'visible',
    ...(glow ? { animation: theme === 'dark' ? 'logo-glow 3s ease-in-out infinite' : 'logo-glow-light 3s ease-in-out infinite' } : {}),
    ...style,
  };

  // One class per layer group: the hero float, the compact drift, or nothing.
  const layer = (name: 'base' | 'ring' | 'top') => {
    if (float) return `logo-float-${name}`;
    if (mini) return `logo-mini-${name}`;
    return undefined;
  };
  const animate = float || mini;

  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox={ISOLA_LOGO_VIEWBOX}
      className={className}
      style={mergedStyle}
      {...(draggable !== undefined ? { draggable } : {})}
    >
      <g transform={ISOLA_LOGO_CENTER}>
        {animate || renderFaces ? (
          <g className={layer('base')}>
            {renderFaces ? renderFaces(faces) : faces}
          </g>
        ) : (
          faces
        )}
        {animate || renderRing ? (
          <g className={layer('ring')}>
            {renderRing ? renderRing(ring) : ring}
          </g>
        ) : (
          ring
        )}
        {animate || renderTop ? (
          <g className={layer('top')}>
            {renderTop ? renderTop(top) : top}
          </g>
        ) : (
          top
        )}
      </g>
    </svg>
  );
}
