import { describe, expect, it } from 'vitest';

import { mockModeFromLocation } from './mode';

describe('mockModeFromLocation', () => {
  it('ignores the URL parameter in a normal build', () => {
    expect(mockModeFromLocation('?mock', undefined, false)).toBeNull();
    expect(mockModeFromLocation('?mock=auth', undefined, false)).toBeNull();
    expect(mockModeFromLocation('?mock=unreachable&gate=fail', '', false)).toBeNull();
    expect(mockModeFromLocation('?mock', '0', false)).toBeNull();
    expect(mockModeFromLocation('', undefined, false)).toBeNull();
  });

  it('follows the URL parameter on the development server', () => {
    expect(mockModeFromLocation('', undefined, true)).toBeNull();
    expect(mockModeFromLocation('?mock', undefined, true)).toBe('open');
    expect(mockModeFromLocation('?mock=1', undefined, true)).toBe('open');
    expect(mockModeFromLocation('?mock=auth', undefined, true)).toBe('auth');
    expect(mockModeFromLocation('?tab=hosts&mock=unreachable', undefined, true)).toBe('unreachable');
  });

  it('mocks every page load of a VITE_MOCK build, production or not', () => {
    expect(mockModeFromLocation('', '1', false)).toBe('open');
    expect(mockModeFromLocation('', '1', true)).toBe('open');
    expect(mockModeFromLocation('', 'auth', false)).toBe('auth');
  });

  it('lets the URL parameter pick the mode of a VITE_MOCK build', () => {
    expect(mockModeFromLocation('?mock=auth', '1', false)).toBe('auth');
    expect(mockModeFromLocation('?mock=unreachable', '1', false)).toBe('unreachable');
    expect(mockModeFromLocation('?mock', 'auth', false)).toBe('open');
  });
});
