const { test } = require('node:test');
const assert = require('node:assert');
const { chromeUserAgent, applyUserAgent, linuxArch } = require('../src/user_agent');

const VERSIONS = { chrome: '156.0.8078.49', electron: '45.1.0' };

test('UA advertises Chrome and never the embedder', () => {
  // The regression guard for "This browser isn't supported": Electron's default
  // UA carries `electron-runner/1.0.0 ... Electron/45.1.0`, which bowser and
  // UAParser resolve to browser "Electron" — rejected by every supported-
  // browser gate even though the engine is a current Chromium.
  const ua = chromeUserAgent(VERSIONS, 'x64');
  assert.equal(
    ua,
    'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.8078.49 Safari/537.36',
  );
  assert.ok(!/electron/i.test(ua), `UA must not name Electron: ${ua}`);
  assert.ok(!ua.includes('electron-runner'), `UA must not name the app: ${ua}`);
});

test('UA keeps the real Chromium version', () => {
  const ua = chromeUserAgent({ chrome: '151.0.7223.12' }, 'arm64');
  assert.ok(ua.includes('Chrome/151.0.7223.12'), ua);
  // Chromium itself writes "Linux aarch64" on arm64; the UA must not pretend
  // the container is x86_64 when navigator.platform already says otherwise.
  assert.ok(ua.includes('(X11; Linux aarch64)'), ua);
});

test('missing Chromium version fails loudly', () => {
  // A silent fallback would ship a UA with Chrome/undefined, which is worse than
  // the Electron string it replaces.
  assert.throws(() => chromeUserAgent({}, 'x64'), /process\.versions\.chrome/);
});

test('arch tokens match Chromium platform spellings', () => {
  assert.equal(linuxArch('x64'), 'x86_64');
  assert.equal(linuxArch('arm64'), 'aarch64');
  assert.equal(linuxArch('arm'), 'armv7l');
  assert.equal(linuxArch('ia32'), 'x86_64');
});

test('applyUserAgent sets the app-wide fallback', () => {
  const app = {};
  const ua = applyUserAgent(app, VERSIONS, 'x64');
  assert.equal(app.userAgentFallback, ua);
  assert.ok(ua.startsWith('Mozilla/5.0 (X11; Linux x86_64)'));
});
