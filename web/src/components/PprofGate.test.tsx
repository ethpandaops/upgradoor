import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { MOCK_AUTH_SERVICE, MOCK_AUTH_URL } from '../mock/mockController';
import { createHarness, type Harness } from '../test/harness';
import { PprofGate, REFUSED_MESSAGE, isPprofPath, pprofCookie, tokenExpiry, type GateEffects } from './PprofGate';

const NOW = Date.UTC(2026, 10, 28, 10, 0, 0);

function effects() {
  return {
    setCookie: vi.fn<(cookie: string) => void>(),
    reload: vi.fn<() => void>(),
    now: () => NOW,
    https: () => true,
  } satisfies GateEffects;
}

function renderGate(h: Harness, fx: GateEffects) {
  const config = {
    network: 'mock-devnet-0',
    version: 'test',
    authProviderUrl: h.auth.getState().enabled ? MOCK_AUTH_URL : '',
    authService: MOCK_AUTH_SERVICE,
  };
  return render(<PprofGate auth={h.auth} config={config} effects={fx} />);
}

/** jwt builds an unsigned token with an exp claim. */
const jwt = (exp: number) => `e30.${btoa(JSON.stringify({ exp })).replace(/=+$/, '')}.sig`;

afterEach(() => {
  sessionStorage.clear();
});

describe('PprofGate', () => {
  it('asks a logged-out visitor to log in, and continues after the login', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: false });
    const fx = effects();
    renderGate(h, fx);

    expect(screen.getByText(/The profiling pages need an admin login/)).toBeInTheDocument();
    expect(fx.setCookie).not.toHaveBeenCalled();

    await userEvent.setup().click(screen.getByRole('button', { name: 'Login' }));

    await waitFor(() => {
      expect(fx.reload).toHaveBeenCalledTimes(1);
    });
    expect(fx.setCookie).toHaveBeenCalledTimes(1);
  });

  it('hands an admin login to the server as a cookie and reloads', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'staff' });
    const fx = effects();
    renderGate(h, fx);

    await waitFor(() => {
      expect(fx.reload).toHaveBeenCalledTimes(1);
    });
    const cookie = fx.setCookie.mock.calls[0]?.[0] ?? '';
    expect(cookie).toMatch(/^upgradoor_debug_token=[^;]+; /);
    expect(cookie).toContain('; Path=/debug/pprof;');
    expect(cookie).toContain('; Max-Age=1800;');
    expect(cookie).toContain('; SameSite=Strict');
    expect(cookie).toMatch(/; Secure$/);
  });

  it('tells a login without admin what it lacks and sets no cookie', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'reader' });
    const fx = effects();
    renderGate(h, fx);

    expect(screen.getByText(`${MOCK_AUTH_SERVICE}:admin`)).toBeInTheDocument();
    await new Promise(resolve => setTimeout(resolve, 0));
    expect(fx.setCookie).not.toHaveBeenCalled();
    expect(fx.reload).not.toHaveBeenCalled();
  });

  it('does not reload again when the server refused the token it just got', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'staff' });
    const token = (await h.auth.getToken()) ?? '';
    sessionStorage.setItem('upgradoor.pprofAttempt', `${token.slice(-16)}:${NOW - 5_000}`);

    const fx = effects();
    renderGate(h, fx);

    expect(await screen.findByRole('alert')).toHaveTextContent(REFUSED_MESSAGE);
    expect(fx.setCookie).not.toHaveBeenCalled();
    expect(fx.reload).not.toHaveBeenCalled();
  });

  it('sets the cookie again after an older attempt', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'staff' });
    const token = (await h.auth.getToken()) ?? '';
    sessionStorage.setItem('upgradoor.pprofAttempt', `${token.slice(-16)}:${NOW - 60_000}`);

    const fx = effects();
    renderGate(h, fx);

    await waitFor(() => {
      expect(fx.reload).toHaveBeenCalledTimes(1);
    });
  });

  it('says so in open mode', async () => {
    const h = await createHarness({ mode: 'open' });
    const fx = effects();
    renderGate(h, fx);

    expect(screen.getByText(/has no login/)).toBeInTheDocument();
    expect(fx.setCookie).not.toHaveBeenCalled();
  });
});

describe('pprofCookie', () => {
  it('never outlives the token', () => {
    const cookie = pprofCookie(jwt(NOW / 1000 + 120), NOW, false);
    expect(cookie).toContain('; Max-Age=120;');
    expect(cookie).not.toContain('Secure');
    expect(tokenExpiry(jwt(1234))).toBe(1_234_000);
    expect(tokenExpiry('opaque')).toBeUndefined();
  });

  it('knows the profiling paths', () => {
    expect(isPprofPath('/debug/pprof/')).toBe(true);
    expect(isPprofPath('/debug/pprof/heap')).toBe(true);
    expect(isPprofPath('/debug/pprofx')).toBe(false);
    expect(isPprofPath('/')).toBe(false);
  });
});
