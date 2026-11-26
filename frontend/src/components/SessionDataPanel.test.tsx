import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

vi.mock('../lib/seedApi', async (importOriginal) => {
  const actual = await importOriginal<typeof SeedApiModule>();
  return { ...actual, applySeed: vi.fn() };
});

import { applySeed } from '../lib/seedApi';
import type * as SeedApiModule from '../lib/seedApi';
import type { SeedResult } from '../lib/seedApi';
import { SessionDataPanel } from './SessionDataPanel';

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('SessionDataPanel', () => {
  it('sends pasted cookies with the reload flag and reports counts', async () => {
    const apply = vi.mocked(applySeed);
    apply.mockResolvedValue({ cookies: 2, headers: 0, storage: 0, reloaded: true });
    const onClose = vi.fn();

    render(<SessionDataPanel sessionId="sid-1" token="tok-1" onClose={onClose} />);
    const applyButton = screen.getByRole('button', { name: 'Apply' });
    expect(applyButton).toHaveProperty('disabled', true);

    fireEvent.change(screen.getByLabelText(/Cookies/), { target: { value: 'sid=abc; theme=dark' } });
    fireEvent.click(screen.getByRole('checkbox'));
    fireEvent.click(applyButton);

    await waitFor(() => expect(apply).toHaveBeenCalledTimes(1));
    expect(apply).toHaveBeenCalledWith(
      'sid-1',
      { cookies: [{ name: 'sid', value: 'abc' }, { name: 'theme', value: 'dark' }], reload: true },
      'tok-1',
    );
    expect(await screen.findByRole('status')).toHaveProperty(
      'textContent',
      expect.stringContaining('Applied 2 cookies'),
    );
    expect(onClose).not.toHaveBeenCalled();
  });

  it('shows the JSON validation error without calling the API', async () => {
    render(<SessionDataPanel sessionId="sid-1" token="tok-1" onClose={vi.fn()} />);
    fireEvent.change(screen.getByLabelText(/Cookies/), { target: { value: '{"cookies":[' } });
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));

    expect((await screen.findByRole('alert')).textContent).toMatch(/Invalid JSON/);
    expect(applySeed).not.toHaveBeenCalled();
  });

  it('surfaces the backend error message', async () => {
    vi.mocked(applySeed).mockRejectedValue(
      new Error('the page is not on this origin'),
    );
    render(<SessionDataPanel sessionId="sid-1" token="tok-1" onClose={vi.fn()} />);
    fireEvent.change(screen.getByLabelText(/Cookies/), { target: { value: 'a=1' } });
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));

    expect((await screen.findByRole('alert')).textContent).toBe('the page is not on this origin');
  });

  it('sends one request when Apply is double-clicked', async () => {
    let release: (value: SeedResult) => void = () => {};
    const apply = vi.mocked(applySeed);
    apply.mockImplementation(() => new Promise((resolve) => { release = resolve; }));

    render(<SessionDataPanel sessionId="sid-1" token="tok-1" onClose={vi.fn()} />);
    fireEvent.change(screen.getByLabelText(/Cookies/), { target: { value: 'a=1' } });
    const button = screen.getByRole('button', { name: 'Apply' });
    fireEvent.click(button);
    fireEvent.click(button);

    expect(apply).toHaveBeenCalledTimes(1);
    release({ cookies: 1, headers: 0, storage: 0, reloaded: false });
    await screen.findByRole('status');
  });

  it('docks as a non-modal panel, not a modal popup', () => {
    render(<SessionDataPanel sessionId="sid-1" token="tok-1" onClose={vi.fn()} />);
    const panel = screen.getByRole('dialog', { name: 'Custom data for this page' });
    // No aria-modal and no backdrop: the live page stays usable behind it.
    expect(panel).not.toHaveAttribute('aria-modal');
  });

  it('closes on Escape', () => {
    const onClose = vi.fn();
    render(<SessionDataPanel sessionId="sid-1" token="tok-1" onClose={onClose} />);
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('disables the reload checkbox for a JSON payload, which owns its own reload', () => {
    render(<SessionDataPanel sessionId="sid-1" token="tok-1" onClose={vi.fn()} />);
    const box = screen.getByRole('checkbox');
    expect(box).toBeEnabled();

    fireEvent.change(screen.getByLabelText(/Cookies/), { target: { value: '{"cookies":[]}' } });
    expect(screen.getByRole('checkbox')).toBeDisabled();
  });
});
