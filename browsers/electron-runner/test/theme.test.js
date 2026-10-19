const { test } = require('node:test');
const assert = require('node:assert/strict');
const { PAGE_CANVAS, themeColours, fillPageCanvas } = require('../src/theme');

// The window backdrop is what Electron composites a page over when the page has
// no background of its own. It must be the browser canvas in every theme; the
// regression made it #1a1a2e in dark mode, so black default text on nginx's 403
// page was unreadable.
test('the window backdrop is the page canvas in every theme', () => {
  for (const theme of ['dark', 'light', 'auto', undefined, '']) {
    assert.equal(themeColours(theme).window, PAGE_CANVAS, `theme ${String(theme)}`);
  }
});

test('a dark theme still opens a dark empty document', () => {
  const dark = themeColours('dark');
  assert.equal(dark.themeSource, 'dark');
  assert.deepEqual(dark.emptyDocument, { r: 26, g: 26, b: 46, a: 255 });
  assert.notEqual(dark.window, '#1a1a2e');
});

test('light and unknown themes resolve to light', () => {
  for (const theme of ['light', 'auto', undefined, '']) {
    const colours = themeColours(theme);
    assert.equal(colours.themeSource, 'light', `theme ${String(theme)}`);
    assert.deepEqual(colours.emptyDocument, { r: 255, g: 255, b: 255, a: 255 });
  }
});

// Only damaged rects are blitted into the compositing surface, so the base fill
// is what shows wherever the page did not paint.
test('the compositing surface is filled opaque white', () => {
  const buf = Buffer.alloc(4 * 4, 0);
  fillPageCanvas(buf);
  for (let i = 0; i < buf.length; i += 4) {
    assert.deepEqual([...buf.subarray(i, i + 4)], [255, 255, 255, 255]);
  }
});
