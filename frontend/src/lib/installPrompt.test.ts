import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { InstallOutcome, InstallState } from './installPrompt';

// The capture lives at module scope, so each case needs a fresh module
// instance; otherwise the first dispatched event is spent for every later one.
interface InstallPromptModule {
  getInstallState: () => InstallState;
  promptInstall: () => Promise<InstallOutcome>;
  dismissInstall: () => void;
  subscribeInstallState: (listener: () => void) => () => void;
}

async function loadModule(): Promise<InstallPromptModule> {
  vi.resetModules();
  return import('./installPrompt');
}

function installEvent(prompt: () => Promise<void>, outcome: 'accepted' | 'dismissed') {
  const event = new Event('beforeinstallprompt') as Event & Record<string, unknown>;
  event.prompt = prompt;
  event.userChoice = Promise.resolve({ outcome, platform: 'web' });
  return event;
}

function stubMatchMedia(standalone: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: standalone && query === '(display-mode: standalone)',
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
}

describe('installPrompt', () => {
  beforeEach(() => {
    localStorage.clear();
    stubMatchMedia(false);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('stays hidden until Chromium hands over a prompt', async () => {
    const mod = await loadModule();
    expect(mod.getInstallState()).toBe('hidden');
    await expect(mod.promptInstall()).resolves.toBe('unavailable');

    window.dispatchEvent(installEvent(async () => {}, 'dismissed'));
    expect(mod.getInstallState()).toBe('ready');
  });

  it('spends the prompt exactly once and then hides', async () => {
    const mod = await loadModule();
    const prompt = vi.fn(async () => {});
    const event = installEvent(prompt, 'accepted');
    const preventDefault = vi.spyOn(event, 'preventDefault');

    window.dispatchEvent(event);
    expect(preventDefault).toHaveBeenCalled();
    expect(mod.getInstallState()).toBe('ready');

    await expect(mod.promptInstall()).resolves.toBe('accepted');
    expect(prompt).toHaveBeenCalledTimes(1);

    // One-shot: the same page load cannot prompt again, so the pill must not
    // come back.
    expect(mod.getInstallState()).toBe('hidden');
    await expect(mod.promptInstall()).resolves.toBe('unavailable');
  });

  it('hides the pill while running as an installed window', async () => {
    stubMatchMedia(true);

    const mod = await loadModule();
    window.dispatchEvent(installEvent(async () => {}, 'dismissed'));

    expect(mod.getInstallState()).toBe('hidden');
  });

  it('hides the pill after the app is installed', async () => {
    const mod = await loadModule();
    window.dispatchEvent(installEvent(async () => {}, 'dismissed'));
    expect(mod.getInstallState()).toBe('ready');

    window.dispatchEvent(new Event('appinstalled'));

    expect(mod.getInstallState()).toBe('hidden');
    await expect(mod.promptInstall()).resolves.toBe('unavailable');
  });

  it('keeps the pill closed after a dismiss, across reloads', async () => {
    const mod = await loadModule();
    window.dispatchEvent(installEvent(async () => {}, 'dismissed'));
    expect(mod.getInstallState()).toBe('ready');

    mod.dismissInstall();

    expect(mod.getInstallState()).toBe('hidden');
    expect(localStorage.getItem('isola_install_dismissed')).toBe('1');

    // A reload re-imports the module and Chromium offers a fresh prompt: the
    // stored preference must still win.
    const reloaded = await loadModule();
    window.dispatchEvent(installEvent(async () => {}, 'accepted'));
    expect(reloaded.getInstallState()).toBe('hidden');
    await expect(reloaded.promptInstall()).resolves.toBe('unavailable');
  });

  it('notifies subscribers when the prompt arrives and when it is spent', async () => {
    const mod = await loadModule();
    const listener = vi.fn();
    const unsubscribe = mod.subscribeInstallState(listener);

    window.dispatchEvent(installEvent(async () => {}, 'dismissed'));
    expect(listener).toHaveBeenCalledTimes(1);

    await mod.promptInstall();
    expect(listener).toHaveBeenCalledTimes(2);

    unsubscribe();
    window.dispatchEvent(new Event('appinstalled'));
    expect(listener).toHaveBeenCalledTimes(2);
  });

  it('reports the browser dialog outcome', async () => {
    const mod = await loadModule();
    window.dispatchEvent(installEvent(async () => {}, 'dismissed'));

    await expect(mod.promptInstall()).resolves.toBe('dismissed');
  });
});
