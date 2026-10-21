import { StrictMode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';

import LandingPage from './LandingPage';

// Node 26 exposes an undefined `localStorage` global unless started with
// --localstorage-file, which shadows jsdom's implementation. Both stores are
// stubbed with a minimal in-memory Storage so the page can read and write them.
function memoryStorage(): Storage {
  const map = new Map<string, string>();
  return {
    get length() {
      return map.size;
    },
    clear: () => map.clear(),
    getItem: (key: string) => (map.has(key) ? map.get(key)! : null),
    key: (index: number) => Array.from(map.keys())[index] ?? null,
    removeItem: (key: string) => {
      map.delete(key);
    },
    setItem: (key: string, value: string) => {
      map.set(key, String(value));
    },
  };
}

// The landing page probes /api/config on mount (and on a 4s interval); it only
// reads ok/status, so a plain stub object keeps the test free of jsdom's
// Response support.
function stubBackend() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => ({ ok: true, status: 200, json: async () => ({}) })),
  );
}

function LocationProbe() {
  const { search } = useLocation();
  return <div data-testid="location">{search}</div>;
}

function RunTarget() {
  const { search } = useLocation();
  return <div data-testid="run">{search}</div>;
}

function renderAt(entry: string, { strict = false }: { strict?: boolean } = {}) {
  const tree = (
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route
          path="/"
          element={
            <>
              <LandingPage />
              <LocationProbe />
            </>
          }
        />
        <Route path="/run" element={<RunTarget />} />
      </Routes>
    </MemoryRouter>
  );
  return render(strict ? <StrictMode>{tree}</StrictMode> : tree);
}

const addressBox = () => screen.getByPlaceholderText('Enter URL...');

const storedLaunchConfig = () => {
  const raw = sessionStorage.getItem('isola_launch_config');
  return raw ? (JSON.parse(raw) as { url?: string }) : null;
};

beforeEach(() => {
  vi.stubGlobal('localStorage', memoryStorage());
  vi.stubGlobal('sessionStorage', memoryStorage());
  stubBackend();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('?q= deep link', () => {
  it('prefills the address box instead of launching', () => {
    renderAt('/?q=example.com');

    expect(addressBox()).toHaveValue('example.com');
    expect(screen.queryByTestId('run')).toBeNull();
    expect(storedLaunchConfig()).toBeNull();
  });

  it('strips the consumed params so a reload does not re-prefill', () => {
    renderAt('/?q=example.com');

    expect(screen.getByTestId('location')).toHaveTextContent(/^$/);
    expect(addressBox()).toHaveValue('example.com');
  });

  it('launches immediately with autorun=1, normalising a bare hostname', async () => {
    renderAt('/?q=example.com&autorun=1');

    // Decode the way RunPage reads it: that is the URL the sandbox opens.
    const runSearch = (await screen.findByTestId('run')).textContent || '';
    expect(new URLSearchParams(runSearch).get('url')).toBe('https://example.com');
    expect(storedLaunchConfig()?.url).toBe('https://example.com');
  });

  it('turns plain text into a search when autorun=1', async () => {
    renderAt('/?q=isola+browser&autorun=1');

    const runSearch = (await screen.findByTestId('run')).textContent || '';
    expect(new URLSearchParams(runSearch).get('url')).toBe(
      'https://duckduckgo.com/?q=isola%20browser',
    );
    expect(storedLaunchConfig()?.url).toBe('https://duckduckgo.com/?q=isola%20browser');
  });

  it('does not launch twice under StrictMode', async () => {
    const setItem = vi.spyOn(sessionStorage, 'setItem');

    renderAt('/?q=example.com&autorun=1', { strict: true });
    await screen.findByTestId('run');

    const launches = setItem.mock.calls.filter(([key]) => key === 'isola_launch_config');
    expect(launches).toHaveLength(1);
  });

  it('ignores a blank q and still consumes the params', () => {
    renderAt('/?q=%20&autorun=1');

    expect(addressBox()).toHaveValue('');
    expect(screen.queryByTestId('run')).toBeNull();
    expect(screen.getByTestId('location')).toHaveTextContent(/^$/);
  });
});
