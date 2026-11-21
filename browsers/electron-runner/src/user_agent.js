// Browser user agent for the isola sandbox.
//
// Electron's default UA advertises the embedder: on this runner it reads
// `Mozilla/5.0 (X11; Linux aarch64) ... electron-runner/1.0.0 Chrome/156.0.8078.49
// Electron/45.1.0 Safari/537.36`. Sites that parse the UA with bowser or
// UAParser resolve that to browser "Electron", which is not on any supported-
// browser list, so a betting/login/streaming site answers with "This browser
// isn't supported" even though the engine underneath is a current Chromium.
// Verified on unibet.com's client-side gate (unsupported-browser module, bowser
// table with an /electron/i rule): same page, same engine, only the UA string
// differs between the blocked and the served render.
//
// The fix is to advertise the engine we actually run — Chromium's own version
// from process.versions.chrome — and nothing about the embedder.
//
// The architecture token is always x86_64, never the container's: every Linux
// Chromium build reports navigator.platform === "Linux x86_64", including the
// arm64 image (verified in a real session container). A UA carrying "aarch64"
// therefore contradicts the engine's own platform string, and strict bot gates
// read that contradiction as a spoof. Linux Chrome only ships x64, so x86_64 is
// also the truthful client description.

// chromeUserAgent builds the UA a stock Chrome of this exact Chromium version
// would send on Linux. `versions` is process.versions (chrome = 156.0.8078.49).
function chromeUserAgent(versions) {
  const chrome = String((versions && versions.chrome) || '').trim();
  if (!chrome) throw new Error('process.versions.chrome is missing: cannot build a truthful user agent');
  return `Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/${chrome} Safari/537.36`;
}

// applyUserAgent sets the app-wide fallback, which is what BrowserWindow
// webContents and session requests use. Must run before any window exists.
function applyUserAgent(app, versions = process.versions) {
  app.userAgentFallback = chromeUserAgent(versions);
  return app.userAgentFallback;
}

module.exports = { chromeUserAgent, applyUserAgent };
