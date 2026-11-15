import './index.css';

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { App, type MockControls } from './App';
import { ApiClient, type FetchFn } from './api/client';
import { AuthStore } from './auth/authStore';
import { applyTheme } from './hooks/useTheme';
import { mockModeFromLocation } from './mock/mode';

async function start(): Promise<void> {
  applyTheme();

  const root = document.getElementById('root');
  if (!root) throw new Error('index.html has no #root element');

  let fetchFn: FetchFn | undefined;
  let mock: MockControls | undefined;

  const mode = mockModeFromLocation(
    window.location.search,
    import.meta.env.VITE_MOCK as string | undefined,
    import.meta.env.DEV
  );
  if (mode) {
    const install = await import('./mock/install');
    const handle = install.installMock(mode, window.location.search);
    const client = handle.client;
    fetchFn = handle.fetch;
    mock = {
      label: mode === 'open' ? 'mock data' : `mock data (${mode})`,
      gateFailing: () => handle.controller.gateFailing,
      setGateFailing: failing => {
        handle.controller.setGateFailing(failing);
      },
      login: client && {
        options: ['staff', 'team', 'reader'],
        current: () => client.profile(),
        set: profile => {
          if (install.isMockProfile(profile)) client.setProfile(profile);
        },
      },
    };
  }

  const auth = new AuthStore();
  const api = new ApiClient({ fetch: fetchFn, tokens: auth });

  createRoot(root).render(
    <StrictMode>
      <App api={api} auth={auth} mock={mock} />
    </StrictMode>
  );
}

void start();
