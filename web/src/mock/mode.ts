// Deciding whether to run against the mock. Kept apart from the mock itself,
// so a normal page load does not download the fixture.

export type MockMode = 'open' | 'auth' | 'unreachable';

/**
 * mockModeFromLocation decides whether (and how) to mock: the URL parameter
 * `mock` wins, then the VITE_MOCK build flag. Null means: talk to the
 * controller.
 *
 * The URL parameter counts only on the development server (`dev`, from
 * import.meta.env.DEV) and in a build made with VITE_MOCK, which shows the
 * fixture anyway. A normal build, the one a controller serves, ignores it:
 * nobody can be handed a link to a real controller's page that shows made-up
 * data.
 */
export function mockModeFromLocation(search: string, buildFlag: string | undefined, dev: boolean): MockMode | null {
  const flag = buildFlag && buildFlag !== '0' ? buildFlag : null;
  const params = new URLSearchParams(search);
  const value = (dev || flag !== null) && params.has('mock') ? params.get('mock') : flag;

  if (value === null) return null;
  if (value === 'auth' || value === 'unreachable') return value;
  return 'open';
}
