# isola interface demo

A static, no-build page that reproduces the isola client experience:
the landing page with `example.com` pre-filled, the four-phase launch overlay,
and the session viewer with the isola tab strip, address bar, session clock and
actions.

It is a demo of the **interface**, not of the sandbox. Clicking **Open** runs a
simulated launch and shows a static drawing of `example.com` — no container is
booted, nothing is streamed, and the page itself is never requested. The demo
says so in the footer and in the `demo` chip in the viewer toolbar.

The demo is deliberately pinned to `example.com`: the address field is
read-only, and the `+` button explains that new tabs are disabled.

```
demo/
├── index.html   landing + launch overlay + viewer chrome
├── demo.css     palette/keyframes copied from the real app
├── demo.js      simulated launch, tab strip, clock, session actions
└── fonts/       Bitcount Prop Single (the landing wordmark)
```

## Hosting on GitHub Pages

`.github/workflows/pages.yml` publishes this directory as the Pages artifact on
every push to `main` that touches `demo/`.

One-time setup: **Settings → Pages → Build and deployment → Source →
GitHub Actions**. The published URL is then:

```
https://<owner>.github.io/<repo>/
```

For this repository that is `https://dm0xa.github.io/isola-browser/`.

## What is real and what is simulated

| Real | Simulated |
| :--- | :--- |
| Landing layout, wordmark and typing animation, address box, settings menu | The launch itself (phase timings, session id, session clock) |
| Viewer toolbar, tab strip, address bar, clock, end/restart controls, full session menu | The "remote" browser: a static drawing of the IANA `example.com` page |
| Reload (a local repaint) and the tab-strip chrome | New tabs and URL editing, disabled with a short notice |
| Theme toggle (same `isola_theme` storage key as the app) | Tor and resolution settings (stored, but no effect) |

Consequences worth knowing:

- **The demo stays on one page.** The address field is read-only and `+` shows a
  short "disabled in the demo" notice, so the viewer cannot navigate away from
  the drawn `example.com`.
- **The page content is hand-drawn**, copied from the IANA example page (the
  book icon and six languages). It makes no request for `example.com` itself.
- **The session menu is drawn in full; Enter fullscreen and Restart session
  work.** Debug overlay, network panel, quality and share are disabled.
- **The +5m control disappears** at the 30:00 cap (and when the clock reaches
  0), mirroring the real viewer's extend availability.
- **It needs network access** for the Tailwind Play CDN. The page content,
  fonts and logo are local.
- **Fullscreen** uses the browser's Fullscreen API; `Esc` exits, as in the app.

## Tour

Each viewer launch shows a small, non-blocking card offering a 30-second tour;
it disappears as soon as you use the demo. The `?` button in the toolbar starts
or replays it at any time.

The tour has five steps — session identity, clock, browser chrome, page area,
session actions. It uses a scrim highlight on desktop and switches to a bottom
sheet on narrow screens. `Esc` closes it, `←`/`→` move between steps, and focus
stays inside the tour while it is open.

## Keeping it in sync

The markup and sizes are intentionally copied from the real components, so UI
changes should be mirrored here:

| Demo piece | Source of truth |
| :--- | :--- |
| Landing | `frontend/src/pages/LandingPage.tsx` |
| Launch overlay | `frontend/src/components/LoadingOverlay.tsx` |
| Logo and animations | `frontend/src/components/IsolaLogo.tsx` |
| Tab strip and address bar | `frontend/src/hooks/useIsolaStream.ts` (`rebuildTabBar`, `TABBAR_PALETTE`) |
| Toolbar and clock | `frontend/src/pages/viewer/ViewerToolbar.tsx`, `frontend/src/components/SessionTimeControl.tsx` |

This is a marketing/demo artifact; it is not tested by CI and carries no
credentials, tokens or build output.
