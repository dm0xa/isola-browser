import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// The manifest is the whole install surface: if a field Chromium requires is
// missing, or an icon it declares does not resolve at the declared size, the
// app silently stops being installable — and nobody notices until they try to
// install it. These assertions are cheap; a broken icon is not.
// vitest runs with the frontend package as its working directory.
const publicDir = resolve(process.cwd(), 'public');

interface ManifestIcon {
  src: string;
  sizes: string;
  type: string;
  purpose?: string;
}

interface Manifest {
  id?: string;
  name?: string;
  short_name?: string;
  start_url?: string;
  scope?: string;
  display?: string;
  background_color?: string;
  theme_color?: string;
  icons?: ManifestIcon[];
}

const manifest: Manifest = JSON.parse(
  readFileSync(resolve(publicDir, 'manifest.webmanifest'), 'utf8'),
);

function pngSize(path: string): string {
  const buf = readFileSync(path);
  // PNG IHDR: 8-byte signature, 4-byte length, "IHDR", then width and height.
  expect(buf.subarray(12, 16).toString('latin1')).toBe('IHDR');
  return `${buf.readUInt32BE(16)}x${buf.readUInt32BE(20)}`;
}

describe('web app manifest', () => {
  it('declares the fields Chromium requires for an installable app', () => {
    expect(manifest.name).toBeTruthy();
    expect(manifest.short_name).toBeTruthy();
    expect(manifest.start_url).toBe('/');
    expect(manifest.scope).toBe('/');
    expect(manifest.display).toBe('standalone');
  });

  it('paints the installed window with the app surfaces', () => {
    // gray-950 (dark, the default theme) and its matching splash colour: a
    // mismatch shows up as a white flash before the first frame.
    expect(manifest.theme_color).toBe('#030712');
    expect(manifest.background_color).toBe('#030712');
  });

  it('ships every declared icon at the size it claims', () => {
    const icons = manifest.icons ?? [];
    expect(icons.length).toBeGreaterThan(0);

    for (const icon of icons) {
      expect(icon.type).toBe('image/png');
      expect(pngSize(resolve(publicDir, icon.src.replace(/^\//, '')))).toBe(icon.sizes);
    }
  });

  it('separates the plain and the maskable artwork', () => {
    const icons = manifest.icons ?? [];
    const purposes = icons.map((i) => i.purpose ?? 'any');

    // One image declared `any maskable` gets the launcher crop applied to the
    // regular artwork, so the corners bleed.
    expect(purposes).not.toContain('any maskable');
    expect(purposes).toContain('any');
    expect(purposes).toContain('maskable');

    const sizes = icons.filter((i) => (i.purpose ?? 'any') === 'any').map((i) => i.sizes);
    expect(sizes).toContain('192x192');
    expect(sizes).toContain('512x512');

    const maskable = icons.find((i) => i.purpose === 'maskable');
    expect(maskable?.sizes).toBe('512x512');
  });
});
