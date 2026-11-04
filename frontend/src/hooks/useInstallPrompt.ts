import { useSyncExternalStore } from 'react';
import {
  getInstallState,
  subscribeInstallState,
  promptInstall,
  dismissInstall,
} from '../lib/installPrompt';

/**
 * True only while Chromium holds an unspent install prompt and the user has not
 * closed the pill. Installed windows, browsers without `beforeinstallprompt`
 * and spent prompts report false, so callers render nothing rather than a
 * control that cannot install anything.
 */
export function useInstallAvailable(): boolean {
  return useSyncExternalStore(subscribeInstallState, () => getInstallState() === 'ready', () => false);
}

export function useInstallPrompt() {
  return { available: useInstallAvailable(), install: promptInstall, dismiss: dismissInstall };
}
