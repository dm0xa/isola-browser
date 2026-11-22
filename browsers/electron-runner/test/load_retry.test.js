const test = require('node:test');
const assert = require('node:assert/strict');

const { ADDRESS_UNREACHABLE, RETRY_WINDOW_MS, shouldRetryLoad } = require('../src/load_retry');

test('retries a main-frame address-unreachable load', () => {
  assert.equal(
    shouldRetryLoad({ code: ADDRESS_UNREACHABLE, isMainFrame: true, url: 'https://ya.ru/' }),
    true,
  );
});

test('does not retry any other failure code', () => {
  for (const code of [-3, -102, -105, -118, -20]) {
    assert.equal(
      shouldRetryLoad({ code, isMainFrame: true, url: 'https://ya.ru/' }),
      false,
      `code ${code} must not retry`,
    );
  }
});

test('does not retry a subframe or a non-page URL', () => {
  assert.equal(
    shouldRetryLoad({ code: ADDRESS_UNREACHABLE, isMainFrame: false, url: 'https://ya.ru/' }),
    false,
  );
  for (const url of ['about:blank', 'data:text/html,x', 'file:///tmp/x', '']) {
    assert.equal(shouldRetryLoad({ code: ADDRESS_UNREACHABLE, isMainFrame: true, url }), false);
  }
});

test('retries one URL only once inside the window, and again after it', () => {
  const url = 'https://ya.ru/';
  const lastRetry = { url, at: 1000 };

  assert.equal(
    shouldRetryLoad({ code: ADDRESS_UNREACHABLE, isMainFrame: true, url, lastRetry, now: 1000 + RETRY_WINDOW_MS - 1 }),
    false,
  );
  assert.equal(
    shouldRetryLoad({ code: ADDRESS_UNREACHABLE, isMainFrame: true, url, lastRetry, now: 1000 + RETRY_WINDOW_MS }),
    true,
  );
  assert.equal(
    shouldRetryLoad({ code: ADDRESS_UNREACHABLE, isMainFrame: true, url: 'https://other.test/', lastRetry, now: 1001 }),
    true,
  );
});
