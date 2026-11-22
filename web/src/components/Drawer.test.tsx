import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { Drawer } from './Drawer';

function renderDrawer() {
  return render(
    <Drawer label="Host" header="h" onClose={() => undefined}>
      body
    </Drawer>
  );
}

const width = () => screen.getByRole('complementary').style.getPropertyValue('--drawer-w');

describe('Drawer', () => {
  const pageWidth = window.innerWidth;
  afterEach(() => {
    window.innerWidth = pageWidth;
  });

  it('resizes from its edge, keeps the width and clamps it to the page', () => {
    window.innerWidth = 1200;
    const { unmount } = renderDrawer();
    expect(width()).toBe('520px');

    const edge = screen.getByRole('separator');
    fireEvent.keyDown(edge, { key: 'ArrowLeft' });
    expect(width()).toBe('552px');
    expect(localStorage.getItem('upgradoor.drawerWidth')).toBe('552');

    unmount();
    renderDrawer();
    expect(width()).toBe('552px');

    fireEvent.keyDown(screen.getByRole('separator'), { key: 'ArrowRight' });
    fireEvent.keyDown(screen.getByRole('separator'), { key: 'ArrowRight' });
    expect(width()).toBe('488px');

    window.innerWidth = 600;
    fireEvent.keyDown(screen.getByRole('separator'), { key: 'ArrowLeft' });
    expect(width()).toBe('360px');
  });

  it('resets to the default width on double-click', () => {
    localStorage.setItem('upgradoor.drawerWidth', '700');
    window.innerWidth = 1200;
    renderDrawer();
    expect(width()).toBe('700px');

    fireEvent.doubleClick(screen.getByRole('separator'));
    expect(width()).toBe('520px');
    expect(localStorage.getItem('upgradoor.drawerWidth')).toBeNull();
  });
});
