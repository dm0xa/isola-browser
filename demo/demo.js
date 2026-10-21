/*
 * isola interface demo.
 *
 * This is a faithful, self-contained simulation of the isola client:
 * the landing page, the launch overlay and the viewer chrome are the same
 * shapes as the real React app, but nothing is provisioned and nothing is
 * streamed. The "remote browser" is a static drawing of example.com, so the
 * demo can be hosted as plain static files and never requests that page.
 *
 * Real app equivalents:
 *   landing page        frontend/src/pages/LandingPage.tsx
 *   launch phases       frontend/src/components/LoadingOverlay.tsx
 *   tab strip           frontend/src/hooks/useIsolaStream.ts (rebuildTabBar)
 *   session clock       frontend/src/components/SessionTimeControl.tsx
 *   session actions     frontend/src/pages/viewer/ViewerToolbar.tsx
 */
(() => {
  'use strict';

  const GITHUB_URL = 'https://github.com/dm0xa/isola-browser';
  const DEMO_URL = 'https://example.com';
  const DEMO_HOST = 'example.com';
  const TOUR_STEPS = [
    {
      target: '#toolbar-left',
      tight: true,
      title: 'Session identity',
      body: 'Every launch gets a short session id and a stream-quality indicator. The demo chip marks this one as simulated — no container was started.',
    },
    {
      target: '#session-clock',
      title: 'Session clock',
      body: 'Ten minutes by default. +5m extends up to 30 minutes; at the cap the extend control disappears.',
    },
    {
      target: '#isola-tabbar',
      title: 'Browser chrome',
      body: 'One pinned example.com tab with back, reload and an address bar. The URL is fixed and new tabs are disabled in this demo.',
    },
    {
      target: '#example-page',
      title: 'The page area',
      body: 'This example.com page is drawn locally — no request is made. In isola this area would be a live, interactive view of a disposable Chromium container’s screen — clicks, typing and scrolling go straight to the remote browser.',
    },
    {
      target: '#toolbar-actions',
      tight: true,
      extraTargets: ['#session-menu'],
      menu: true,
      title: 'Session actions',
      body: 'Audio, the session menu and End. Only fullscreen and restart work here; in the real product the menu also has network capture, debug and share links, and End destroys the container.',
    },
  ];
  const SESSION_SECONDS = 10 * 60; // session.default_min
  const EXTEND_SECONDS = 5 * 60; // session.extend_min
  const MAX_SECONDS = 30 * 60; // session.max_min
  const PHASE_DELAYS_MS = [650, 700, 850, 350];

  const $ = (id) => document.getElementById(id);

  const el = {
    landing: $('landing'),
    viewer: $('viewer'),
    overlay: $('launch-overlay'),
    launchPhases: $('launch-phases'),
    launchRestart: $('launch-restart'),
    launchCaption: $('launch-caption'),
    form: $('launch-form'),
    urlInput: $('url-input'),
    openBtn: $('open-btn'),
    toast: $('demo-toast'),
    themeToggle: $('theme-toggle'),
    typed: $('typed'),
    cursor: $('cursor'),
    settingsBtn: $('settings-btn'),
    settingsMenu: $('settings-menu'),
    settingsWrap: $('settings-wrap'),
    myScreenValue: $('my-screen-value'),
    torToggle: $('tor-toggle'),
    torIcon: $('tor-icon'),
    torLabel: $('tor-label'),
    sessionId: $('session-id'),
    tabbar: $('isola-tabbar'),
    timeLeft: $('time-left'),
    clockIcon: $('clock-icon'),
    extendBtn: $('extend-btn'),
    extendDivider: $('extend-divider'),
    audioBtn: $('audio-btn'),
    audioOn: $('audio-on'),
    audioOff: $('audio-off'),
    menuBtn: $('menu-btn'),
    sessionMenu: $('session-menu'),
    fullscreenItem: $('fullscreen-item'),
    fullscreenLabel: $('fullscreen-label'),
    fullscreenIcon: $('fullscreen-icon'),
    restartItem: $('restart-item'),
    endBtn: $('end-btn'),
    viewerHome: $('viewer-home'),
    examplePage: $('example-page'),
    tourHelp: $('tour-help'),
    tourIntro: $('tour-intro'),
    tourIntroStart: $('tour-intro-start'),
    tourIntroSkip: $('tour-intro-skip'),
    tourLayer: $('tour-layer'),
    tourHighlight: $('tour-highlight'),
    tourTip: $('tour-tip'),
    tourStep: $('tour-step'),
    tourTitle: $('tour-title'),
    tourBody: $('tour-body'),
    tourBack: $('tour-back'),
    tourNext: $('tour-next'),
    tourSkip: $('tour-skip'),
    tourClose: $('tour-close'),
  };

  const state = {
    theme: 'dark',
    torEnabled: false,
    resolution: '1920x1080',
    sessionId: '',
    remaining: SESSION_SECONDS,
    clock: null,
    extendTimer: null,
    extendMaxed: false,
    endArmed: false,
    endDisarmTimer: null,
    launchToken: 0,
    restarting: false,
    toastTimer: null,
    tourActive: false,
    tourStep: 0,
    tourIntroTimer: null,
  };

  // ── Icons ────────────────────────────────────────────────────────────────

  const SUN_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="w-5 h-5"><circle cx="12" cy="12" r="5"/><path d="M12 1v2M12 21v2M4.22 4.22l1.42 1.42M18.36 18.36l1.42 1.42M1 12h2M21 12h2M4.22 19.78l1.42-1.42M18.36 5.64l1.42-1.42"/></svg>`;
  const MOON_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="w-5 h-5"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>`;

  const TOR_OFF_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" class="w-4 h-4"><circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="1.2"/><circle cx="12" cy="12" r="5.5" fill="none" stroke="currentColor" stroke-width="1"/><circle cx="12" cy="12" r="2.5" fill="none" stroke="currentColor" stroke-width="0.8"/></svg>`;
  const TOR_ON_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" class="w-4 h-4"><circle cx="12" cy="12" r="9" fill="currentColor" opacity="0.15"/><circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="1.5"/><circle cx="12" cy="12" r="5.5" fill="none" stroke="currentColor" stroke-width="1.2"/><circle cx="12" cy="12" r="2.5" fill="currentColor" opacity="0.6"/></svg>`;

  const BACK_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 12H5"/><polyline points="12 19 5 12 12 5"/></svg>`;
  const RELOAD_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-3-6.7"/><polyline points="21 3 21 9 15 9"/></svg>`;
  const CHECK_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="20 6 9 17 4 12"/></svg>`;
  const MAXIMIZE_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="15 3 21 3 21 9"/><polyline points="9 21 3 21 3 15"/><line x1="21" x2="14" y1="3" y2="10"/><line x1="3" x2="10" y1="21" y2="14"/></svg>`;
  const MINIMIZE_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="4 14 10 14 10 20"/><polyline points="20 10 14 10 14 4"/><line x1="14" x2="21" y1="10" y2="3"/><line x1="3" x2="10" y1="21" y2="14"/></svg>`;

  function randomHex(len) {
    const bytes = new Uint8Array(Math.ceil(len / 2));
    crypto.getRandomValues(bytes);
    return Array.from(bytes, (b) => b.toString(16).padStart(2, '0'))
      .join('')
      .slice(0, len);
  }

  const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

  // ── Theme ────────────────────────────────────────────────────────────────

  function applyTheme(theme) {
    state.theme = theme;
    document.documentElement.classList.toggle('dark', theme === 'dark');
    el.themeToggle.innerHTML = theme === 'dark' ? SUN_SVG : MOON_SVG;
    el.themeToggle.title = theme === 'dark' ? 'Switch to light' : 'Switch to dark';
    try {
      localStorage.setItem('isola_theme', theme);
    } catch (e) {
      /* storage unavailable — the choice just won't persist */
    }
  }

  function initTheme() {
    let stored = null;
    try {
      stored = localStorage.getItem('isola_theme');
    } catch (e) {
      /* storage unavailable */
    }
    applyTheme(stored === 'light' ? 'light' : 'dark');
    el.themeToggle.addEventListener('click', () => {
      applyTheme(state.theme === 'dark' ? 'light' : 'dark');
    });
  }

  // ── Landing page ─────────────────────────────────────────────────────────

  function initTyping() {
    const text = 'isola';
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      el.typed.textContent = text;
      return;
    }
    let i = 0;
    const interval = setInterval(() => {
      if (i > text.length) {
        clearInterval(interval);
        return;
      }
      el.typed.textContent = text.slice(0, i);
      i += 1;
    }, 180);
  }

  function initCursor() {
    let on = true;
    setInterval(() => {
      on = !on;
      el.cursor.classList.toggle('cursor-off', !on);
    }, 530);
  }

  function initSettings() {
    el.myScreenValue.textContent = `${Math.round(window.screen.width * window.devicePixelRatio)}x${Math.round(
      window.screen.height * window.devicePixelRatio,
    )}`;

    const setMenuOpen = (open) => {
      el.settingsMenu.toggleAttribute('data-hidden', !open);
      el.settingsBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
    };

    el.settingsBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      setMenuOpen(el.settingsMenu.hasAttribute('data-hidden'));
    });

    document.addEventListener('mousedown', (e) => {
      if (!el.settingsWrap.contains(e.target)) setMenuOpen(false);
    });

    for (const item of el.settingsMenu.querySelectorAll('[data-resolution]')) {
      item.addEventListener('click', () => {
        state.resolution = item.dataset.resolution;
        for (const other of el.settingsMenu.querySelectorAll('[data-resolution]')) {
          other.classList.toggle('is-active', other === item);
        }
      });
    }

    const renderTor = () => {
      el.torIcon.innerHTML = state.torEnabled ? TOR_ON_SVG : TOR_OFF_SVG;
      el.torLabel.textContent = state.torEnabled ? 'Enabled' : 'Disabled';
      el.torLabel.classList.toggle('tor-on', state.torEnabled);
      el.torToggle.setAttribute('aria-pressed', state.torEnabled ? 'true' : 'false');
    };
    renderTor();
    el.torToggle.addEventListener('click', () => {
      state.torEnabled = !state.torEnabled;
      renderTor();
    });
  }

  function isEditKey(event) {
    return event.key.length === 1 || event.key === 'Backspace' || event.key === 'Delete';
  }

  function showToast(message) {
    el.toast.textContent = message;
    el.toast.removeAttribute('data-hidden');
    clearTimeout(state.toastTimer);
    state.toastTimer = setTimeout(() => el.toast.setAttribute('data-hidden', ''), 2400);
  }

  function initLanding() {
    el.urlInput.addEventListener('focus', () => el.urlInput.select());
    el.urlInput.addEventListener('keydown', (e) => {
      if (isEditKey(e)) showToast('The URL is fixed in the demo.');
    });

    el.form.addEventListener('submit', (e) => {
      e.preventDefault();
      void launch();
    });
  }

  // ── Launch overlay ───────────────────────────────────────────────────────

  const phaseRows = () => Array.from(el.launchPhases.children);

  function setPhaseState(row, phase) {
    const icon = row.querySelector('[data-phase-icon]');
    const label = row.querySelector('[data-phase-label]');
    const progress = row.querySelector('[data-phase-progress]');

    icon.textContent = '';
    const marker = document.createElement('span');
    if (phase === 'done') {
      marker.className = 'loader-phase-done';
      marker.textContent = '✓';
      marker.setAttribute('aria-hidden', 'true');
    } else if (phase === 'active') {
      marker.className = 'loader-phase-dot inline-block w-1.5 h-1.5 rounded-full';
      marker.style.animation = 'loader-dot 1.2s ease-in-out infinite';
      marker.setAttribute('aria-hidden', 'true');
    } else {
      marker.className = 'loader-phase-pending';
      marker.textContent = '○';
      marker.setAttribute('aria-hidden', 'true');
    }
    icon.appendChild(marker);

    label.className =
      phase === 'done' ? 'loader-phase-done-text' : phase === 'active' ? 'loader-phase-active' : 'loader-phase-pending';
    progress.toggleAttribute('data-hidden', phase !== 'active');
  }

  function resetPhases() {
    for (const row of phaseRows()) setPhaseState(row, 'pending');
  }

  async function runPhases() {
    const rows = phaseRows();
    for (let i = 0; i < rows.length; i += 1) {
      rows.forEach((row, index) => {
        setPhaseState(row, index < i ? 'done' : index === i ? 'active' : 'pending');
      });
      await wait(PHASE_DELAYS_MS[i]);
    }
    rows.forEach((row) => setPhaseState(row, 'done'));
  }

  async function launch() {
    state.launchToken += 1;
    const token = state.launchToken;

    el.settingsMenu.setAttribute('data-hidden', '');
    el.landing.setAttribute('data-hidden', '');
    el.viewer.setAttribute('data-hidden', '');
    el.sessionMenu.setAttribute('data-hidden', '');

    el.launchCaption.textContent = 'STARTING REMOTE BROWSER';
    el.launchPhases.removeAttribute('data-hidden');
    el.launchRestart.setAttribute('data-hidden', '');
    resetPhases();
    el.overlay.removeAttribute('data-hidden');

    await runPhases();
    if (token !== state.launchToken) return;
    openViewer();
  }

  // ── Viewer: tabs, clock, actions ─────────────────────────────────────────

  // The demo has one fixed page, so "reload" re-paints it instead of fetching.
  function reloadPage() {
    el.examplePage.classList.add('is-reloading');
    el.examplePage.scrollTop = 0;
    setTimeout(() => el.examplePage.classList.remove('is-reloading'), 160);
  }

  function tabButton(html, extraClass, title, onActivate) {
    const button = document.createElement('div');
    button.className = `isola-tabbar-btn${extraClass ? ` ${extraClass}` : ''}`;
    button.setAttribute('role', 'button');
    button.setAttribute('tabindex', '0');
    button.title = title;
    button.innerHTML = html;
    button.addEventListener('mousedown', (e) => {
      e.preventDefault();
      e.stopPropagation();
      onActivate();
    });
    button.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        onActivate();
      }
    });
    return button;
  }

  function renderTabbar() {
    el.tabbar.textContent = '';

    const chip = document.createElement('div');
    chip.className = 'isola-tab active';
    chip.setAttribute('role', 'tab');
    chip.setAttribute('aria-selected', 'true');
    chip.title = DEMO_URL;

    const label = document.createElement('span');
    label.className = 'isola-tab-label';
    label.textContent = DEMO_HOST;

    const close = document.createElement('span');
    close.className = 'isola-tab-close';
    close.textContent = '✕';
    close.title = 'Close tab';
    // Chrome keeps the last tab open; the demo has exactly one.
    close.addEventListener('mousedown', (e) => {
      e.preventDefault();
      e.stopPropagation();
    });

    chip.append(label, close);
    el.tabbar.appendChild(chip);

    el.tabbar.appendChild(
      tabButton('+', 'isola-tabbar-plus', 'New tabs are disabled in the demo', () =>
        showToast('New tabs are disabled in the demo.'),
      ),
    );
    el.tabbar.appendChild(tabButton(BACK_SVG, 'isola-tabbar-back', 'Back', () => {}));
    el.tabbar.appendChild(tabButton(RELOAD_SVG, 'isola-tabbar-reload', 'Reload page', reloadPage));

    const input = document.createElement('input');
    input.type = 'text';
    input.className = 'isola-url';
    input.placeholder = 'Search or enter address';
    input.autocomplete = 'off';
    input.spellcheck = false;
    input.setAttribute('aria-label', 'Address');
    input.readOnly = true;
    input.title = 'The URL is fixed in the demo';
    input.setAttribute('aria-readonly', 'true');
    input.value = DEMO_URL;
    input.addEventListener('focus', () => input.select());
    input.addEventListener('keydown', (e) => {
      e.stopPropagation();
      if (isEditKey(e)) showToast('The URL is fixed in the demo.');
    });
    el.tabbar.appendChild(input);
  }

  function formatTime(totalSec) {
    if (totalSec <= 0) return '0:00';
    const minutes = Math.floor(totalSec / 60);
    const seconds = totalSec % 60;
    return `${minutes}:${seconds.toString().padStart(2, '0')}`;
  }

  function updateClock() {
    el.timeLeft.textContent = formatTime(state.remaining);
    // The real viewer hides the extend control once the session cannot be
    // extended further (the 30-minute cap) or the clock has run out.
    const extendable = !state.extendMaxed && state.remaining > 0;
    el.extendBtn.toggleAttribute('data-hidden', !extendable);
    el.extendDivider.toggleAttribute('data-hidden', !extendable);
    const urgency = state.remaining <= 60 ? 'critical' : state.remaining <= 180 ? 'warning' : '';
    el.timeLeft.classList.toggle('is-warning', urgency === 'warning');
    el.timeLeft.classList.toggle('is-critical', urgency === 'critical');
    el.clockIcon.classList.toggle('is-warning', urgency === 'warning');
    el.clockIcon.classList.toggle('is-critical', urgency === 'critical');
    el.clockIcon.classList.toggle('is-pulsing', urgency === 'critical');
  }

  function stopClock() {
    if (state.clock !== null) {
      clearInterval(state.clock);
      state.clock = null;
    }
  }

  function startClock() {
    stopClock();
    updateClock();
    state.clock = setInterval(() => {
      state.remaining = Math.max(0, state.remaining - 1);
      updateClock();
      if (state.remaining === 0) stopClock();
    }, 1000);
  }

  function resetExtendButton() {
    clearTimeout(state.extendTimer);
    el.extendBtn.innerHTML = '<span>+5m</span>';
    el.extendBtn.classList.remove('is-added');
  }

  function initExtend() {
    el.extendBtn.addEventListener('click', () => {
      if (state.remaining <= 0 || state.extendMaxed) return;
      state.remaining = Math.min(MAX_SECONDS, state.remaining + EXTEND_SECONDS);
      if (state.remaining >= MAX_SECONDS) state.extendMaxed = true;
      updateClock();
      if (state.extendMaxed) return;
      el.extendBtn.innerHTML = `${CHECK_SVG}<span>ok</span>`;
      el.extendBtn.classList.add('is-added');
      clearTimeout(state.extendTimer);
      state.extendTimer = setTimeout(resetExtendButton, 1800);
    });
  }

  function initAudio() {
    let muted = true;
    el.audioBtn.addEventListener('click', () => {
      muted = !muted;
      el.audioOff.toggleAttribute('data-hidden', !muted);
      el.audioOn.toggleAttribute('data-hidden', muted);
      el.audioBtn.classList.toggle('is-on', !muted);
      const label = muted ? 'Unmute audio' : 'Mute audio';
      el.audioBtn.title = label;
      el.audioBtn.setAttribute('aria-label', label);
      el.audioBtn.setAttribute('aria-pressed', muted ? 'false' : 'true');
    });
  }

  function setSessionMenuOpen(open) {
    el.sessionMenu.toggleAttribute('data-hidden', !open);
    el.menuBtn.classList.toggle('is-open', open);
    el.menuBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
  }

  async function toggleFullscreen() {
    setSessionMenuOpen(false);
    try {
      if (!document.fullscreenElement) {
        await el.viewer.requestFullscreen();
      } else {
        await document.exitFullscreen();
      }
    } catch (e) {
      /* fullscreen can be refused — the demo keeps working windowed */
    }
  }

  function renderFullscreenState() {
    const fullscreen = !!document.fullscreenElement;
    el.fullscreenLabel.textContent = fullscreen ? 'Exit fullscreen' : 'Enter fullscreen';
    el.fullscreenIcon.innerHTML = fullscreen ? MINIMIZE_SVG : MAXIMIZE_SVG;
  }

  function initSessionMenu() {
    el.menuBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      setSessionMenuOpen(el.sessionMenu.hasAttribute('data-hidden'));
    });
    document.addEventListener('mousedown', (e) => {
      if (state.tourActive) return;
      if (!el.sessionMenu.contains(e.target) && !el.menuBtn.contains(e.target)) setSessionMenuOpen(false);
    });

    el.fullscreenItem.addEventListener('click', () => void toggleFullscreen());

    el.restartItem.addEventListener('click', () => {
      void restartSession();
    });

    document.addEventListener('fullscreenchange', renderFullscreenState);
  }

  async function restartSession() {
    if (state.restarting) return;
    state.restarting = true;
    setSessionMenuOpen(false);

    el.launchCaption.textContent = 'RESTARTING SESSION';
    el.launchPhases.setAttribute('data-hidden', '');
    el.launchRestart.removeAttribute('data-hidden');
    el.overlay.removeAttribute('data-hidden');

    await wait(1500);

    el.overlay.setAttribute('data-hidden', '');
    el.launchRestart.setAttribute('data-hidden', '');
    el.launchPhases.removeAttribute('data-hidden');

    state.remaining = SESSION_SECONDS;
    state.extendMaxed = false;
    resetExtendButton();
    startClock();

    reloadPage();
    renderTabbar();
    state.restarting = false;
  }

  function disarmEnd() {
    state.endArmed = false;
    clearTimeout(state.endDisarmTimer);
    el.endBtn.classList.remove('is-armed');
    el.endBtn.title = 'End session';
    el.endBtn.setAttribute('aria-label', 'End session');
  }

  function endSession() {
    disarmEnd();
    stopClock();
    resetExtendButton();
    setSessionMenuOpen(false);
    el.tabbar.textContent = '';
    endTour(false);
    hideTourIntro();

    if (document.fullscreenElement) {
      document.exitFullscreen().catch(() => {});
    }

    el.viewer.setAttribute('data-hidden', '');
    el.overlay.setAttribute('data-hidden', '');
    el.landing.removeAttribute('data-hidden');
  }

  function initEndButton() {
    el.endBtn.addEventListener('click', () => {
      if (!state.endArmed) {
        state.endArmed = true;
        el.endBtn.classList.add('is-armed');
        el.endBtn.title = 'Click again to end session';
        el.endBtn.setAttribute('aria-label', 'Click again to end session');
        clearTimeout(state.endDisarmTimer);
        state.endDisarmTimer = setTimeout(disarmEnd, 1800);
        return;
      }
      endSession();
    });

    // Any click outside the button disarms it, like the real toolbar.
    window.addEventListener(
      'mousedown',
      (e) => {
        if (!state.endArmed) return;
        if (el.endBtn.contains(e.target)) return;
        disarmEnd();
      },
      true,
    );
  }

  function initViewerHome() {
    el.viewerHome.addEventListener('click', endSession);
  }

  // ── Interface tour ───────────────────────────────────────────────────────

  function hideTourIntro() {
    clearTimeout(state.tourIntroTimer);
    el.tourIntro.setAttribute('data-hidden', '');
  }

  function showTourIntro() {
    hideTourIntro();
    state.tourIntroTimer = setTimeout(() => {
      if (state.tourActive) return;
      if (el.viewer.hasAttribute('data-hidden')) return;
      el.tourIntro.removeAttribute('data-hidden');
    }, 700);
  }

  // A flex-1 toolbar group is wider than its content; the tour highlights the
  // union of its children instead of the empty space.
  function targetRect(target, tight) {
    if (!tight) return target.getBoundingClientRect();
    const children = Array.from(target.children).filter((child) => child.getClientRects().length > 0);
    if (children.length === 0) return target.getBoundingClientRect();
    const rects = children.map((child) => child.getBoundingClientRect());
    const left = Math.min(...rects.map((r) => r.left));
    const top = Math.min(...rects.map((r) => r.top));
    const right = Math.max(...rects.map((r) => r.right));
    const bottom = Math.max(...rects.map((r) => r.bottom));
    return { top, left, right, bottom, width: right - left, height: bottom - top };
  }

  function unionRect(a, b) {
    const left = Math.min(a.left, b.left);
    const top = Math.min(a.top, b.top);
    const right = Math.max(a.right, b.right);
    const bottom = Math.max(a.bottom, b.bottom);
    return { top, left, right, bottom, width: right - left, height: bottom - top };
  }

  function positionTourStep() {
    if (!state.tourActive) return;
    const step = TOUR_STEPS[state.tourStep];
    const target = document.querySelector(step.target);
    if (!target) return;

    const rect = (() => {
      let value = targetRect(target, step.tight);
      for (const selector of step.extraTargets || []) {
        const extra = document.querySelector(selector);
        if (extra) value = unionRect(value, extra.getBoundingClientRect());
      }
      return value;
    })();
    const pad = 6;
    el.tourHighlight.style.top = `${Math.max(0, rect.top - pad)}px`;
    el.tourHighlight.style.left = `${Math.max(0, rect.left - pad)}px`;
    el.tourHighlight.style.width = `${Math.min(window.innerWidth, rect.width + pad * 2)}px`;
    el.tourHighlight.style.height = `${Math.min(window.innerHeight, rect.height + pad * 2)}px`;

    // A tall target (the page) or a narrow viewport gets a bottom sheet.
    const sheet = window.innerWidth < 640 || rect.height > window.innerHeight * 0.55;
    el.tourTip.classList.toggle('is-sheet', sheet);
    el.tourTip.style.left = '';
    el.tourTip.style.top = '';
    el.tourTip.style.right = '';
    el.tourTip.style.bottom = '';

    if (sheet) {
      el.tourTip.style.bottom = '12px';
      return;
    }

    const tipWidth = el.tourTip.offsetWidth;
    const tipHeight = el.tourTip.offsetHeight;
    let left = rect.left + rect.width / 2 - tipWidth / 2;
    left = Math.max(8, Math.min(left, window.innerWidth - tipWidth - 8));
    let top = rect.bottom + 12;
    if (top + tipHeight > window.innerHeight - 8) top = rect.top - tipHeight - 12;
    if (top < 8) top = Math.max(8, window.innerHeight - tipHeight - 8);
    el.tourTip.style.left = `${Math.round(left)}px`;
    el.tourTip.style.top = `${Math.round(top)}px`;
  }

  function showTourStep(index) {
    const step = TOUR_STEPS[index];
    if (!step) return;
    state.tourStep = index;
    el.tourStep.textContent = `${index + 1} / ${TOUR_STEPS.length}`;
    el.tourTitle.textContent = step.title;
    el.tourBody.textContent = step.body;
    el.tourBack.disabled = index === 0;
    el.tourNext.textContent = index === TOUR_STEPS.length - 1 ? 'Done' : 'Next';
    if (step.menu) {
      el.sessionMenu.classList.add('is-tour-enabled');
      setSessionMenuOpen(true);
    } else {
      el.sessionMenu.classList.remove('is-tour-enabled');
      setSessionMenuOpen(false);
    }
    positionTourStep();
  }

  function tourNext() {
    if (!state.tourActive) return;
    if (state.tourStep >= TOUR_STEPS.length - 1) {
      endTour();
      return;
    }
    showTourStep(state.tourStep + 1);
    el.tourNext.focus();
  }

  function tourBack() {
    if (!state.tourActive || state.tourStep <= 0) return;
    showTourStep(state.tourStep - 1);
    el.tourNext.focus();
  }

  function trapTourFocus(event) {
    const focusables = Array.from(el.tourTip.querySelectorAll('button:not([disabled])'));
    if (focusables.length === 0) return;
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  function onTourKeydown(event) {
    if (event.key === 'Escape') {
      event.preventDefault();
      endTour();
      return;
    }
    if (event.key === 'ArrowRight') {
      event.preventDefault();
      tourNext();
      return;
    }
    if (event.key === 'ArrowLeft') {
      event.preventDefault();
      tourBack();
      return;
    }
    if (event.key === 'Tab') trapTourFocus(event);
  }

  function repositionTour() {
    requestAnimationFrame(positionTourStep);
  }

  function startTour() {
    hideTourIntro();
    if (el.viewer.hasAttribute('data-hidden')) return;

    setSessionMenuOpen(false);
    el.sessionMenu.classList.remove('is-tour-enabled');
    state.tourActive = true;
    state.tourStep = 0;
    el.tourLayer.removeAttribute('data-hidden');
    document.addEventListener('keydown', onTourKeydown);
    window.addEventListener('resize', repositionTour);
    document.addEventListener('fullscreenchange', repositionTour);
    showTourStep(0);
    el.tourNext.focus();
  }

  function endTour(restoreFocus = true) {
    if (!state.tourActive) return;
    state.tourActive = false;
    el.tourLayer.setAttribute('data-hidden', '');
    el.sessionMenu.classList.remove('is-tour-enabled');
    setSessionMenuOpen(false);
    document.removeEventListener('keydown', onTourKeydown);
    window.removeEventListener('resize', repositionTour);
    document.removeEventListener('fullscreenchange', repositionTour);
    if (restoreFocus && !el.viewer.hasAttribute('data-hidden')) el.tourHelp.focus();
  }

  function initTour() {
    el.tourHelp.addEventListener('click', startTour);
    el.tourIntroStart.addEventListener('click', startTour);
    el.tourIntroSkip.addEventListener('click', () => {
      hideTourIntro();
    });
    el.tourNext.addEventListener('click', tourNext);
    el.tourBack.addEventListener('click', tourBack);
    el.tourSkip.addEventListener('click', () => endTour());
    el.tourClose.addEventListener('click', () => endTour());

    // Using the demo dismisses the card for this session.
    el.viewer.addEventListener(
      'mousedown',
      (event) => {
        if (el.tourIntro.hasAttribute('data-hidden')) return;
        if (el.tourIntro.contains(event.target)) return;
        hideTourIntro();
      },
      true,
    );
  }

  function openViewer() {
    state.sessionId = randomHex(8);
    el.sessionId.textContent = state.sessionId;

    state.remaining = SESSION_SECONDS;
    state.extendMaxed = false;
    resetExtendButton();
    startClock();

    el.overlay.setAttribute('data-hidden', '');
    el.landing.setAttribute('data-hidden', '');
    el.viewer.removeAttribute('data-hidden');
    setSessionMenuOpen(false);
    disarmEnd();

    renderTabbar();
    showTourIntro();
  }

  // ── Boot ─────────────────────────────────────────────────────────────────

  function init() {
    initTheme();
    initTyping();
    initCursor();
    initSettings();
    initLanding();
    initExtend();
    initAudio();
    initSessionMenu();
    initEndButton();
    initViewerHome();
    initTour();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  // Expose the demo state for the browser console / link-out tooling.
  window.isolaDemo = { state, GITHUB_URL };
})();
