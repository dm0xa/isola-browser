import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  buildTabOptions,
  IsolaNetworkPanel,
  methodOf,
  statusGroupOf,
  type BrowserTab,
  type ConsoleEntry,
  type CookieEntry,
  type NetEntry,
  type PanelTab,
} from './IsolaNetworkPanel';

const tab = (id: number, url: string, active = false): BrowserTab => ({ id, url, title: '', active });
const net = (tb: number | undefined, url: string): NetEntry => ({ id: `r${tb}-${url}`, url, m: 'GET', s: 200, t: 'Document', tb });

// Renders are not auto-cleaned (vitest runs without `globals`), so each test
// would otherwise append another panel to the document.
afterEach(cleanup);

describe('buildTabOptions', () => {
  it('names each tab by its domain', () => {
    const options = buildTabOptions(
      [tab(1, 'https://shop.example.com/'), tab(2, 'https://example.com/page')],
      [],
      [],
    );

    expect(options.map((o) => o.label)).toEqual(['shop.example.com', 'example.com']);
    expect(options.map((o) => o.id)).toEqual([1, 2]);
  });

  it('strips a leading www', () => {
    const options = buildTabOptions([tab(1, 'https://www.example.com/')], [], []);

    expect(options[0].label).toBe('example.com');
  });

  it('numbers tabs that share a domain, so options stay distinct', () => {
    const options = buildTabOptions(
      [tab(1, 'https://shop.example.com/'), tab(2, 'https://shop.example.com/search')],
      [],
      [],
    );

    expect(options.map((o) => o.label)).toEqual(['shop.example.com (1)', 'shop.example.com (2)']);
  });

  it('lists a freshly opened tab even when it produced no captured traffic', () => {
    const options = buildTabOptions(
      [tab(1, 'https://shop.example.com/'), tab(2, 'https://news.example.com/')],
      [net(1, 'https://shop.example.com/')],
      [],
    );

    expect(options.map((o) => o.id)).toEqual([1, 2]);
    expect(options.map((o) => o.label)).toEqual(['shop.example.com', 'news.example.com']);
  });

  it('falls back to the most common host when the tab URL is a placeholder', () => {
    const entries = [
      net(1, 'https://example.com/a'),
      net(1, 'https://example.com/b'),
      net(1, 'https://cdn.other.example/x'),
    ];
    const options = buildTabOptions([tab(1, 'about:blank')], entries, []);

    expect(options[0].label).toBe('example.com');
  });

  it('includes tab ids seen only in captured events', () => {
    const options = buildTabOptions([tab(1, 'https://shop.example.com/')], [net(3, 'https://late.example/x')], []);

    expect(options.map((o) => o.id)).toEqual([1, 3]);
    expect(options.map((o) => o.label)).toEqual(['shop.example.com', 'late.example']);
  });

  it('derives a domain from traffic captured before any tab was active', () => {
    const options = buildTabOptions([], [net(-1, 'https://shop.example.com/')], []);

    expect(options).toHaveLength(1);
    expect(options[0].label).toBe('shop.example.com');
  });

  it('labels unattributed traffic with no derivable host', () => {
    const options = buildTabOptions([], [net(-1, 'data:text/plain,hi')], []);

    expect(options[0].label).toBe('unattributed');
  });

  it('marks a tab with no domain and no traffic as "tab N"', () => {
    const options = buildTabOptions([tab(7, 'about:blank')], [], []);

    expect(options[0].label).toBe('tab 7');
  });

  it('counts console-only tabs too', () => {
    const consoleEntries: ConsoleEntry[] = [{ level: 'log', text: 'hi', tb: 4 }];
    const options = buildTabOptions([], [], consoleEntries);

    expect(options.map((o) => o.id)).toEqual([4]);
  });
});

describe('IsolaNetworkPanel tab filter', () => {
  const renderPanel = (tabs: BrowserTab[], entries: NetEntry[]) => {
    render(
      <IsolaNetworkPanel
        entries={entries}
        consoleEntries={[]}
        cookieEntries={[]}
        layout="bottom"
        tab="network"
        onTab={() => {}}
        onLayout={() => {}}
        onClose={() => {}}
        tabs={tabs}
      />,
    );
    return screen.getAllByRole('option').map((o) => o.textContent);
  };

  it('renders one option per open tab, named by domain', () => {
    const options = renderPanel(
      [tab(1, 'https://shop.example.com/'), tab(2, 'https://news.example.com/')],
      [net(1, 'https://shop.example.com/a'), net(2, 'https://news.example.com/b')],
    );

    expect(options).toEqual(['all tabs', 'shop.example.com', 'news.example.com']);
  });

  it('lists a newly opened tab before it produces any traffic', () => {
    const options = renderPanel(
      [tab(1, 'https://shop.example.com/'), tab(2, 'https://news.example.com/')],
      [net(1, 'https://shop.example.com/a')],
    );

    expect(options).toEqual(['all tabs', 'shop.example.com', 'news.example.com']);
  });

  it('disambiguates two tabs on the same domain', () => {
    const options = renderPanel(
      [tab(1, 'https://shop.example.com/'), tab(2, 'https://shop.example.com/search')],
      [net(1, 'https://shop.example.com/a'), net(2, 'https://shop.example.com/b')],
    );

    expect(options).toEqual(['all tabs', 'shop.example.com (1)', 'shop.example.com (2)']);
  });
});

describe('methodOf / statusGroupOf', () => {
  const req = (over: Partial<NetEntry>): NetEntry => ({
    id: 'x',
    url: 'https://example.com/',
    m: 'GET',
    s: 200,
    t: 'Document',
    ...over,
  });

  it('defaults a missing method to GET and uppercases it', () => {
    expect(methodOf(req({ m: '' }))).toBe('GET');
    expect(methodOf(req({ m: 'post' }))).toBe('POST');
  });

  it('buckets status codes into their family', () => {
    expect(statusGroupOf(req({ s: 204 }))).toBe('2xx');
    expect(statusGroupOf(req({ s: 301 }))).toBe('3xx');
    expect(statusGroupOf(req({ s: 404 }))).toBe('4xx');
    expect(statusGroupOf(req({ s: 503 }))).toBe('5xx');
  });

  it('files a failed request under ERR regardless of status', () => {
    expect(statusGroupOf(req({ s: 0, f: true }))).toBe('ERR');
  });

  it('files a response that never arrived under other', () => {
    expect(statusGroupOf(req({ s: 0 }))).toBe('other');
  });
});

describe('IsolaNetworkPanel method/status filters', () => {
  const req = (over: Partial<NetEntry>): NetEntry => ({
    id: over.id || 'x',
    url: 'https://example.com/',
    m: 'GET',
    s: 200,
    t: 'Document',
    ...over,
  });

  const setup = (entries: NetEntry[]) => {
    render(
      <IsolaNetworkPanel
        entries={entries}
        consoleEntries={[]}
        cookieEntries={[]}
        layout="bottom"
        tab="network"
        onTab={() => {}}
        onLayout={() => {}}
        onClose={() => {}}
      />,
    );
  };

  // Each request row renders the host in its own span, so counting those spans
  // counts the visible rows.
  const rowCount = () => screen.queryAllByText('example.com').length;

  it('narrows rows to the clicked method and restores them when cleared', () => {
    setup([
      req({ id: 'a', url: 'https://example.com/a', m: 'GET' }),
      req({ id: 'b', url: 'https://example.com/b', m: 'POST' }),
      req({ id: 'c', url: 'https://example.com/c', m: 'POST' }),
    ]);

    expect(rowCount()).toBe(3);

    fireEvent.click(screen.getByRole('button', { name: /POST/ }));
    expect(rowCount()).toBe(2);

    fireEvent.click(screen.getByRole('button', { name: /GET/ }));
    expect(rowCount()).toBe(3);

    fireEvent.click(screen.getByRole('button', { name: /clear/ }));
    expect(rowCount()).toBe(3);
  });

  it('combines method and status filters', () => {
    setup([
      req({ id: 'a', url: 'https://example.com/a', m: 'GET', s: 200 }),
      req({ id: 'b', url: 'https://example.com/b', m: 'GET', s: 404 }),
      req({ id: 'c', url: 'https://example.com/c', m: 'POST', s: 200 }),
    ]);

    fireEvent.click(screen.getByRole('button', { name: /GET/ }));
    expect(rowCount()).toBe(2);

    fireEvent.click(screen.getByRole('button', { name: /2xx/ }));
    expect(rowCount()).toBe(1);
  });

  it('filters rows by resource type and restores them with clear', () => {
    setup([
      req({ id: 'a', url: 'https://example.com/app.js', t: 'Script' }),
      req({ id: 'b', url: 'https://example.com/style.css', t: 'Stylesheet' }),
      req({ id: 'c', url: 'https://example.com/logo.png', t: 'Image' }),
    ]);

    expect(rowCount()).toBe(3);

    fireEvent.click(screen.getByTitle('Filter by Script'));
    expect(rowCount()).toBe(1);

    // Types accumulate: Script + Image.
    fireEvent.click(screen.getByTitle('Filter by Image'));
    expect(rowCount()).toBe(2);

    fireEvent.click(screen.getByRole('button', { name: /clear/ }));
    expect(rowCount()).toBe(3);
  });

  it('shows the streamer SHA-1 in the row detail with a copy affordance', () => {
    setup([req({ id: 'a', url: 'https://example.com/app.js', t: 'Script', h: 'abcdef0123456789abcdef0123456789abcdef01' })]);

    fireEvent.click(screen.getByTitle('https://example.com/app.js'));

    expect(screen.getByText('abcdef0123456789abcdef0123456789abcdef01')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Copy SHA-1/ })).toBeInTheDocument();
  });

  it('shows a truncated SHA-1 in every row so files can be compared without expanding', () => {
    setup([
      req({ id: 'a', url: 'https://example.com/app.js', t: 'Script', h: 'abcdef0123456789abcdef0123456789abcdef01' }),
      req({ id: 'b', url: 'https://example.com/style.css', t: 'Stylesheet', h: '0123456789abcdef0123456789abcdef01234567' }),
    ]);

    // Header column plus one truncated digest per row, visible with no click.
    expect(screen.getByText('sha1')).toBeInTheDocument();
    expect(screen.getByText('abcdef012345…')).toBeInTheDocument();
    expect(screen.getByText('0123456789ab…')).toBeInTheDocument();
  });

  it('leaves the SHA-1 column empty for a file the streamer could not hash', () => {
    setup([req({ id: 'a', url: 'https://example.com/app.js', t: 'Script' })]);

    expect(screen.getByTitle(/no hash/)).toBeInTheDocument();
  });

  it('copies the row as JSON with the SHA-1 the streamer captured', () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    setup([req({ id: 'a', url: 'https://example.com/app.js', t: 'Script', h: 'abcdef0123456789abcdef0123456789abcdef01' })]);

    fireEvent.click(screen.getByTitle('https://example.com/app.js'));
    fireEvent.click(screen.getByRole('button', { name: /Copy JSON/ }));

    const copied = JSON.parse((writeText.mock.calls[0] as unknown as [string])[0]);
    expect(copied.sha1).toBe('abcdef0123456789abcdef0123456789abcdef01');
  });

  it('keeps the sha1 field in the copied JSON even when no hash was captured', () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    setup([req({ id: 'a', url: 'https://example.com/app.js', t: 'Script' })]);

    fireEvent.click(screen.getByTitle('https://example.com/app.js'));
    fireEvent.click(screen.getByRole('button', { name: /Copy JSON/ }));

    const copied = JSON.parse((writeText.mock.calls[0] as unknown as [string])[0]);
    expect(copied).toHaveProperty('sha1');
    expect(copied.sha1).toBe('');
  });
});

describe('IsolaNetworkPanel copy action', () => {
  const cookie = (over: Partial<CookieEntry> = {}): CookieEntry => ({
    domain: 'example.com',
    name: 'sid',
    value: 'abc123',
    path: '/',
    sameSite: 'Lax',
    expires: '',
    httpOnly: true,
    secure: true,
    ...over,
  });

  const setup = (props: {
    tab: PanelTab;
    entries?: NetEntry[];
    consoleEntries?: ConsoleEntry[];
    cookieEntries?: CookieEntry[];
  }) => {
    render(
      <IsolaNetworkPanel
        entries={props.entries ?? []}
        consoleEntries={props.consoleEntries ?? []}
        cookieEntries={props.cookieEntries ?? []}
        layout="bottom"
        tab={props.tab}
        onTab={() => {}}
        onLayout={() => {}}
        onClose={() => {}}
      />,
    );
  };

  const clipboard = () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    return writeText;
  };

  it('copies cookies, not network rows, when the cookies tab is active', () => {
    const writeText = clipboard();
    setup({ tab: 'cookies', entries: [net(1, 'https://example.com/api')], cookieEntries: [cookie()] });

    fireEvent.click(screen.getByTitle('Copy all cookies as TSV to clipboard'));

    expect(writeText).toHaveBeenCalledWith(
      'name\tvalue\tdomain\tpath\texpires\tsamesite\thttponly\tsecure\n' +
        'sid\tabc123\texample.com\t/\tSession\tLax\ttrue\ttrue',
    );
  });

  it('copies console lines, escaping characters that would break a TSV record', () => {
    const writeText = clipboard();
    setup({
      tab: 'console',
      entries: [net(1, 'https://example.com/api')],
      consoleEntries: [{ level: 'error', text: 'boom\nnext', tb: 1 }],
    });

    fireEvent.click(screen.getByTitle('Copy visible console lines as TSV to clipboard'));

    expect(writeText).toHaveBeenCalledWith('time\tlevel\ttext\n\terror\tboom\\nnext');
  });

  it('copies request rows as headered TSV with ISO times and raw byte counts', () => {
    const writeText = clipboard();
    setup({
      tab: 'network',
      entries: [{ ...net(1, 'https://example.com/api'), ts: Date.UTC(2026, 0, 2, 3, 4, 5, 678), b: 1234, t: 'XHR' }],
    });

    fireEvent.click(screen.getByTitle('Copy visible rows as TSV to clipboard'));

    expect(writeText).toHaveBeenCalledWith(
      'time\tmethod\tstatus\tbytes\ttype\tip\tsha1\terror\turl\n' +
        '2026-01-02T03:04:05.678Z\tGET\t200\t1234\tXHR\t\t\t\thttps://example.com/api',
    );
  });
});
