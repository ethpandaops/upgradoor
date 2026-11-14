// Deciding whether to run against the mock. Kept apart from the mock itself,
// so a normal page load does not download the fixture.

export type MockMode = 'open' | 'auth' | 'unreachable';

/**
 * mockModeFromLocation decides whether (and how) to mock: the URL parameter
 * `mock` wins, then the VITE_MOCK build flag. Null means: talk to the
 * controller.
 */
export function mockModeFromLocation(search: string, buildFlag: string | undefined): MockMode | null {
  const params = new URLSearchParams(search);
  const value = params.has('mock') ? params.get('mock') : buildFlag && buildFlag !== '0' ? buildFlag : null;

  if (value === null) return null;
  if (value === 'auth' || value === 'unreachable') return value;
  return 'open';
}
