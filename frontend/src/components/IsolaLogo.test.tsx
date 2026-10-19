import { cleanup, render } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { IsolaLogo } from './IsolaLogo';

afterEach(cleanup);

const LAYERS = ['base', 'ring', 'top'] as const;

describe('IsolaLogo animation layers', () => {
  it('is static by default', () => {
    const { container } = render(<IsolaLogo />);
    for (const name of LAYERS) {
      expect(container.querySelector(`.logo-float-${name}`)).toBeNull();
      expect(container.querySelector(`.logo-mini-${name}`)).toBeNull();
    }
  });

  it('floats the hero layers', () => {
    const { container } = render(<IsolaLogo float />);
    for (const name of LAYERS) {
      expect(container.querySelector(`.logo-float-${name}`)).not.toBeNull();
    }
  });

  // The 36px chrome logo (viewer and share toolbars): the hero offsets are
  // sub-pixel at that size, so it uses the compact drift instead.
  it('uses the compact drift for the chrome logo', () => {
    const { container } = render(<IsolaLogo mini />);
    for (const name of LAYERS) {
      expect(container.querySelector(`.logo-mini-${name}`)).not.toBeNull();
      expect(container.querySelector(`.logo-float-${name}`)).toBeNull();
    }
  });

  it('pulses the glow unless disabled', () => {
    const { container, rerender } = render(<IsolaLogo />);
    expect(container.querySelector('svg')?.style.animation).toContain('logo-glow');

    rerender(<IsolaLogo glow={false} />);
    expect(container.querySelector('svg')?.style.animation).toBe('');
  });
});
