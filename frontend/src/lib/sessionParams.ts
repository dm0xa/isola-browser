// Session parameters the viewer polls from GET /api/sessions/{id}.
//
// Kept in one shape so the debug report and the id tooltip cannot describe the
// session differently. Values are the *stored* session facts, which is what a
// support report needs: a blank timezone/locale means the sandbox is on the
// image default (UTC / en-US), not "unknown".

export type SessionParams = {
  resolution: string;
  frameRate: number;
  browserType: string;
  timezone: string;
  locale: string;
  torEnabled: boolean;
  torCountry: string;
};

export const EMPTY_SESSION_PARAMS: SessionParams = {
  resolution: '',
  frameRate: 0,
  browserType: '',
  timezone: '',
  locale: '',
  torEnabled: false,
  torCountry: '',
};

/**
 * Hover text for the session-id chip in the toolbar. One line per dimension so
 * a native tooltip stays readable, with explicit defaults instead of blanks.
 */
export function sessionParamsTooltip(id: string | undefined, params: SessionParams): string {
  const frameRate = params.frameRate > 0 ? `${params.frameRate} fps` : 'fps default';
  return [
    `session ${id || '?'}`,
    `${params.resolution || 'resolution ?'} · ${frameRate} · ${params.browserType || 'browser ?'}`,
    `timezone ${params.timezone || 'UTC (default)'} · locale ${params.locale || 'en-US (default)'}`,
    params.torEnabled ? `tor on${params.torCountry ? ` (${params.torCountry})` : ''}` : 'tor off',
  ].join('\n');
}

/**
 * Folds one GET /api/sessions/{id} payload into the cached parameters.
 *
 * Returns the previous object when nothing changed: the viewer polls at 1 Hz
 * and a fresh object every tick would re-render the toolbar and the memoised
 * stream shell around it. Fields the response omits (they are `omitempty`
 * server-side) keep their previous value instead of flickering to a default.
 */
export function mergeSessionParams(prev: SessionParams, data: Record<string, unknown>): SessionParams {
  const next: SessionParams = {
    resolution: typeof data.resolution === 'string' ? data.resolution : prev.resolution,
    frameRate: typeof data.frame_rate === 'number' ? data.frame_rate : prev.frameRate,
    browserType: typeof data.browser_type === 'string' ? data.browser_type : prev.browserType,
    timezone: typeof data.timezone === 'string' ? data.timezone : prev.timezone,
    locale: typeof data.locale === 'string' ? data.locale : prev.locale,
    torEnabled: typeof data.tor_enabled === 'boolean' ? data.tor_enabled : prev.torEnabled,
    torCountry: typeof data.tor_country === 'string' ? data.tor_country : prev.torCountry,
  };
  const unchanged =
    next.resolution === prev.resolution &&
    next.frameRate === prev.frameRate &&
    next.browserType === prev.browserType &&
    next.timezone === prev.timezone &&
    next.locale === prev.locale &&
    next.torEnabled === prev.torEnabled &&
    next.torCountry === prev.torCountry;
  return unchanged ? prev : next;
}
