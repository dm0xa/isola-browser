// Chromium's "install as an app" surface.
//
// The `beforeinstallprompt` event is the only way a page can offer the native
// install dialog, and `prompt()` may only be called on that event, from a user
// gesture. The listener is therefore registered at module scope — an effect
// inside a component can mount too late and miss it.
//
// Two states, deliberately: the control is shown only while Chromium holds an
// unspent prompt, so clicking it always opens the dialog. It is not shown when
// Chromium has not handed one over (it withholds the event on its own terms —
// engagement, a previous dismissal, an origin that looks installed), because a
// button that cannot install anything is worse than no button; the browser's
// own address-bar install entry covers that case.
//
// The event object is one-shot: once `prompt()` has been called Chromium will
// not fire another one this page load, so the reference is dropped before the
// call and the control disappears instead of offering a dead button.

export interface BeforeInstallPromptEvent extends Event {
  prompt: () => Promise<void>;
  userChoice: Promise<{ outcome: 'accepted' | 'dismissed'; platform: string }>;
}

export type InstallOutcome = 'accepted' | 'dismissed' | 'unavailable';
export type InstallState = 'ready' | 'hidden';

let deferred: BeforeInstallPromptEvent | null = null;
// The prompt is offered at most once per page load; after it has been shown the
// control must not come back.
let spent = false;
const listeners = new Set<() => void>();

// Dismissing the install affordance is a preference, not a per-page decision:
// it has to survive reloads and route changes, so it lives in localStorage next
// to the other UI preferences (isola_theme, isola_tor). Chromium's own install
// icon in the address bar stays available either way.
const DISMISS_KEY = 'isola_install_dismissed';
let dismissed = typeof window === 'undefined' ? false : readDismissed();

function readDismissed(): boolean {
  try {
    return window.localStorage.getItem(DISMISS_KEY) === '1';
  } catch {
    // Storage can be unavailable (private mode, disabled cookies); treat that
    // as "not dismissed" rather than breaking the page.
    return false;
  }
}

function emit(): void {
  for (const listener of listeners) listener();
}

function isStandalone(): boolean {
  return typeof window.matchMedia === 'function'
    && window.matchMedia('(display-mode: standalone)').matches;
}

if (typeof window !== 'undefined') {
  window.addEventListener('beforeinstallprompt', (event) => {
    // Chromium's own mini-prompt is suppressed in favour of the app's control.
    event.preventDefault();
    deferred = event as BeforeInstallPromptEvent;
    emit();
  });

  window.addEventListener('appinstalled', () => {
    deferred = null;
    spent = true;
    emit();
  });

  if (typeof window.matchMedia === 'function') {
    const standalone = window.matchMedia('(display-mode: standalone)');
    standalone.addEventListener?.('change', () => {
      if (standalone.matches) {
        deferred = null;
        emit();
      }
    });
  }
}

/** Opens the browser's install dialog. Returns how the user answered. */
export async function promptInstall(): Promise<InstallOutcome> {
  // A dismissed pill means the app does not offer installation any more, so no
  // code path may pop the dialog behind the user's back.
  if (dismissed) return 'unavailable';
  const event = deferred;
  if (!event) return 'unavailable';
  // One-shot: drop the reference before awaiting so a second click cannot reuse
  // an event Chromium already consumed.
  deferred = null;
  spent = true;
  emit();
  try {
    await event.prompt();
    const choice = await event.userChoice;
    return choice.outcome;
  } catch {
    return 'dismissed';
  }
}

/** Hides the install affordance for good (until storage is cleared). */
export function dismissInstall(): void {
  if (dismissed) return;
  dismissed = true;
  try {
    window.localStorage.setItem(DISMISS_KEY, '1');
  } catch {
    // Preference is still honoured for this page load.
  }
  emit();
}

export function subscribeInstallState(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function getInstallState(): InstallState {
  // The user closed the pill: respect that even if Chromium offers a prompt.
  if (dismissed || spent) return 'hidden';
  // The app may already be running as an installed window: there is nothing to
  // offer then, whatever Chromium reports.
  if (isStandalone()) return 'hidden';
  return deferred ? 'ready' : 'hidden';
}
