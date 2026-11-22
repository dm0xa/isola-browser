// Retry policy for a main-frame load that failed because the first resolved
// address was unreachable.
//
// The sandbox has no IPv6 route, and its egress policy drops IPv6. When a host
// has an AAAA record (ya.ru is a reliable example), Chromium's first navigation
// can pick that address, the connect fails with ENETUNREACH, and the whole load
// is reported as ERR_ADDRESS_UNREACHABLE. Chromium has by then cached the
// address as failing, so loading the same URL again succeeds over IPv4 — which
// is what made the failure look intermittent to users.
//
// Retry exactly once per URL. Everything else stays a real error: a refused
// connection, a DNS failure, or a blocked URL must reach the user unchanged.

const ADDRESS_UNREACHABLE = -106;
const RETRY_WINDOW_MS = 5000;

function shouldRetryLoad({ code, isMainFrame, url, lastRetry, now = Date.now() }) {
  if (code !== ADDRESS_UNREACHABLE || !isMainFrame) return false;
  // Only a real page can be reloaded; about:blank, data: and file: failures
  // are not address-family races.
  if (typeof url !== 'string' || !/^https?:\/\//i.test(url)) return false;
  if (lastRetry && lastRetry.url === url && now - lastRetry.at < RETRY_WINDOW_MS) {
    return false;
  }
  return true;
}

module.exports = { ADDRESS_UNREACHABLE, RETRY_WINDOW_MS, shouldRetryLoad };
