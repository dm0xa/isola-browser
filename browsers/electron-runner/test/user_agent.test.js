const { test } = require('node:test');
const assert = require('node:assert');
const { chromeUserAgent, applyUserAgent } = require('../src/user_agent');

const VERSIONS = { chrome: '156.0.8078.49', electron: '45.1.0' };

test('UA advertises Chrome and never the embedder', () => {
  // The regression guard for "This browser isn't supported": Electron's default
  // UA carries `electron-runner/1.0.0 ... Electron/45.1.0`, which bowser and
  // UAParser resolve to browser "Electron" — rejected by every supported-
  // browser gate even though the engine is a current Chromium.
  const ua = chromeUserAgent(VERSIONS);
  assert.equal(
    ua,
    'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.8078.49 Safari/537.36',
  );
  assert.ok(!/electron/i.test(ua), `UA must not name Electron: ${ua}`);
  assert.ok(!ua.includes('electron-runner'), `UA must not name the app: ${ua}`);
});

test('UA keeps the real Chromium version', () => {
  const ua = chromeUserAgent({ chrome: '151.0.7223.12' });
  assert.ok(ua.includes('Chrome/151.0.7223.12'), ua);
});

test('UA never contradicts navigator.platform', () => {
  // On the arm64 image Chromium still reports navigator.platform
  // "Linux x86_64" (verified in a real session container). A UA claiming
  // aarch64/armv7l is a spoof signal, so the token is always x86_64.
  for (const versions of [{ chrome: '156.0.8078.49' }, { chrome: '151.0.7223.12' }]) {
    const ua = chromeUserAgent(versions);
    assert.ok(ua.includes('(X11; Linux x86_64)'), ua);
    assert.ok(!/aarch64|armv7l/.test(ua), `UA must not carry an arch Chromium contradicts: ${ua}`);
  }
});

test('missing Chromium version fails loudly', () => {
  // A silent fallback would ship a UA with Chrome/undefined, which is worse than
  // the Electron string it replaces.
  assert.throws(() => chromeUserAgent({}), /process\.versions\.chrome/);
});

test('applyUserAgent sets the app-wide fallback', () => {
  const app = {};
  const ua = applyUserAgent(app, VERSIONS);
  assert.equal(app.userAgentFallback, ua);
  assert.ok(ua.startsWith('Mozilla/5.0 (X11; Linux x86_64)'));
});
