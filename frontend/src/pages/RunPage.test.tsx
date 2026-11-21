import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import RunPage from './RunPage';

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

describe('RunPage launch request', () => {
  beforeEach(() => {
    vi.stubGlobal('sessionStorage', memoryStorage());
    vi.stubGlobal('localStorage', memoryStorage());
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('posts the picked resolution instead of one fitted to the window', async () => {
    // The warm pool only claims a container baked at exactly this resolution,
    // so the create request must carry the launcher's preset. Fitting it to the
    // window here (this one would come out 1824x1080) made every launch cold.
    Object.defineProperty(document.documentElement, 'clientWidth', { value: 1440, configurable: true });
    Object.defineProperty(window, 'innerHeight', { value: 900, configurable: true });
    sessionStorage.setItem(
      'isola_launch_config',
      JSON.stringify({ url: 'https://example.com', resolution: '1920x1080', browser_theme: 'dark' }),
    );

    const bodies: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: unknown, init?: { body?: string }) => {
        if (init?.body) bodies.push(init.body);
        return { ok: true, status: 200, json: async () => ({ id: 'sess-1', session_token: 'tok' }) };
      }),
    );

    render(
      <MemoryRouter initialEntries={['/run?url=https%3A%2F%2Fexample.com']}>
        <Routes>
          <Route path="/run" element={<RunPage />} />
          <Route path="/session/:id" element={<div />} />
        </Routes>
      </MemoryRouter>,
    );

    await waitFor(() => expect(bodies.length).toBeGreaterThan(0));
    const body = JSON.parse(bodies[0]) as { resolution?: string };
    expect(body.resolution).toBe('1920x1080');
  });
});
