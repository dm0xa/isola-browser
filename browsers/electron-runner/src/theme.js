// Colour decisions for the offscreen surface.
//
// Two surfaces, two rules:
//
//   - the WINDOW backdrop is what Electron composites a page without its own
//     background over, so it is the browser canvas — white — in every theme;
//   - an EMPTY document (about:blank / a new tab) is ours to colour and follows
//     the theme, so a dark session opens a dark tab.
//
// Sharing one colour between them is the bug this module exists to prevent.
// With the dark theme colour as the backdrop, any page that relies on the
// canvas instead of painting a background (nginx's 403, text/plain) rendered
// Chromium's black default text on #1a1a2e.

const PAGE_CANVAS = '#ffffff';

const DARK_EMPTY_DOCUMENT = { r: 26, g: 26, b: 46, a: 255 };
const LIGHT_EMPTY_DOCUMENT = { r: 255, g: 255, b: 255, a: 255 };

// themeColours resolves one theme value into the colours the runner applies.
// Unknown values (including "auto", which the backend resolves before it
// reaches the container) fall back to light.
function themeColours(theme) {
  if (theme === 'dark') {
    return {
      themeSource: 'dark',
      window: PAGE_CANVAS,
      emptyDocument: DARK_EMPTY_DOCUMENT,
    };
  }
  return {
    themeSource: 'light',
    window: PAGE_CANVAS,
    emptyDocument: LIGHT_EMPTY_DOCUMENT,
  };
}

// fillPageCanvas resets the compositing surface to opaque white. The runner
// only blits damaged rects into this buffer, so whatever is left here shows
// wherever the page did not paint.
function fillPageCanvas(buf) {
  buf.fill(255); // r = g = b = a = 255
  return buf;
}

module.exports = { PAGE_CANVAS, themeColours, fillPageCanvas };
