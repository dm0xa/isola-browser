import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { IsolaNetworkPanel, type CookieEntry, type NetEntry } from './IsolaNetworkPanel';
import type { DocumentHeaders, IntelResponse } from '../lib/intel';

afterEach(cleanup);

type Props = Parameters<typeof IsolaNetworkPanel>[0];

const baseProps = (over: Partial<Props> = {}): Props => ({
  entries: [],
  consoleEntries: [],
  cookieEntries: [],
  layout: 'bottom',
  tab: 'network',
  onTab: () => {},
  onLayout: () => {},
  onClose: () => {},
  ...over,
});

const doc = (over: Partial<DocumentHeaders> = {}): DocumentHeaders => ({
  id: 't:1',
  url: 'https://example.com/',
  s: 200,
  hd: { 'strict-transport-security': 'max-age=31536000', server: 'nginx' },
  pst: {
    score: 3,
    total: 7,
    grade: 'C',
    checks: [
      { name: 'hsts', present: true, value: 'max-age=31536000' },
      { name: 'csp', present: false },
    ],
  },
  tls: { protocol: 'TLS 1.3', issuer: 'Test CA', valid_from: 1700000000, valid_to: 1800000000 },
  ...over,
});

describe('IsolaNetworkPanel headers tab', () => {
  it('renders document headers, posture grade and certificate', () => {
    render(<IsolaNetworkPanel {...baseProps({ tab: 'headers', docHeaders: { 't:1': doc() } })} />);

    expect(screen.getByText('nginx')).toBeTruthy();
    expect(screen.getByText('max-age=31536000')).toBeTruthy();
    expect(screen.getByText(/C · 3\/7/)).toBeTruthy();
    expect(screen.getByText(/TLS 1.3/)).toBeTruthy();
    expect(screen.getByText(/✓ hsts/)).toBeTruthy();
    expect(screen.getByText(/✗ csp/)).toBeTruthy();
  });

  it('shows an empty state before any document is captured', () => {
    render(<IsolaNetworkPanel {...baseProps({ tab: 'headers' })} />);
    expect(screen.getByText(/No document headers captured yet/)).toBeTruthy();
  });
});

describe('IsolaNetworkPanel trackers tab', () => {
  const entry = (over: Partial<NetEntry>): NetEntry => ({
    id: 'x',
    url: 'https://example.com/',
    m: 'GET',
    s: 200,
    t: 'Document',
    ...over,
  });

  it('groups tracker families, third-party hosts, beacons and cookie families', () => {
    const entries: NetEntry[] = [
      entry({ id: '1', url: 'https://www.google-analytics.com/collect', t: 'Ping', tr: 'Google Analytics', tp: true, bc: true }),
      entry({ id: '2', url: 'https://cdn.other.example/app.js', tp: true }),
      entry({ id: '3' }),
    ];
    const cookieEntries: CookieEntry[] = [
      { domain: '.example.com', name: '_ga', value: 'x', path: '/', sameSite: 'Lax', expires: '', httpOnly: false, secure: true, fp: 'Google Analytics' },
    ];

    render(<IsolaNetworkPanel {...baseProps({ tab: 'trackers', entries, cookieEntries })} />);

    expect(screen.getAllByText('Google Analytics').length).toBeGreaterThan(0);
    expect(screen.getAllByText('www.google-analytics.com').length).toBeGreaterThan(0);
    expect(screen.getByText(/2 third-party hosts/)).toBeTruthy();
    expect(screen.getByText(/1 beacons/)).toBeTruthy();
    expect(screen.getByText('_ga')).toBeTruthy();
  });
});

describe('IsolaNetworkPanel recon tab', () => {
  it('runs the selected probes through the injected fetcher', async () => {
    const response: IntelResponse = {
      host: 'example.com',
      probes: {
        robots: { check: 'robots', status: 'ok', http_status: 200, data: { disallow: ['/admin'] } },
      },
    };
    const onIntel = vi.fn().mockResolvedValue(response);

    render(<IsolaNetworkPanel {...baseProps({
      tab: 'recon',
      onIntel,
      tabs: [{ id: 1, url: 'https://example.com/', title: '', active: true }],
    })} />);

    fireEvent.click(screen.getByTitle(/Run the selected probes/));

    await waitFor(() => expect(screen.getByText('robots')).toBeTruthy());
    expect(onIntel).toHaveBeenCalledWith({
      host: 'example.com',
      checks: 'robots,securitytxt,sitemap',
      mode: 'probe',
    });
    expect(screen.getByText(/disallow/)).toBeTruthy();
  });

  it('explains when the operator disabled recon', () => {
    render(<IsolaNetworkPanel {...baseProps({ tab: 'recon', intelDisabled: true, onIntel: vi.fn() })} />);
    expect(screen.getByText(/disabled by the operator/)).toBeTruthy();
    expect((screen.getByTitle(/Run the selected probes/) as HTMLButtonElement).disabled).toBe(true);
  });

  it('offers only same-origin probes', () => {
    render(<IsolaNetworkPanel {...baseProps({ tab: 'recon', onIntel: vi.fn() })} />);
    expect(screen.getByText('robots.txt')).toBeTruthy();
    expect(screen.getByText('favicon hash')).toBeTruthy();
    expect(screen.queryByText(/RDAP/)).toBeNull();
    expect(screen.queryByText(/Certificate logs/)).toBeNull();
    expect(screen.queryByText(/Wayback/)).toBeNull();
  });
});

describe('IsolaNetworkPanel redirect chain', () => {
  it('shows each hop with its status code in the expanded row', () => {
    const entry: NetEntry = {
      id: 't:1',
      url: 'https://example.com/',
      m: 'GET',
      s: 200,
      t: 'Document',
      rd: [{ status: 301, from: 'http://example.com/', to: 'https://example.com/' }],
      ttfb: 42,
    };
    render(<IsolaNetworkPanel {...baseProps({ entries: [entry] })} />);

    fireEvent.click(screen.getByText('example.com'));
    expect(screen.getByText('redirect chain')).toBeTruthy();
    expect(screen.getByText('301')).toBeTruthy();
    expect(screen.getByText('ttfb 42 ms')).toBeTruthy();
  });
});

describe('IsolaNetworkPanel transport trust', () => {
  it('marks a certificate Chromium accepted without trusting, and explains the capped grade', () => {
    render(
      <IsolaNetworkPanel
        {...baseProps({
          tab: 'headers',
          docHeaders: {
            't:1': doc({
              tls: { protocol: 'TLS 1.3', issuer: 'Zscaler', security_state: 'insecure' },
              pst: {
                score: 7,
                total: 7,
                grade: 'C',
                tls_state: 'insecure',
                note: 'certificate chain was not trusted (Chromium accepts certificate errors); grade capped',
                checks: [{ name: 'hsts', present: true }],
              },
            }),
          },
        })}
      />,
    );

    expect(screen.getByText('untrusted cert')).toBeTruthy();
    expect(screen.getByText(/grade capped/)).toBeTruthy();
  });
});
