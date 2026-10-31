import React from 'react';
import ReactDOM from 'react-dom/client';
import * as ReactJSXRuntime from 'react/jsx-runtime';
import * as ReactRouterDOM from 'react-router-dom';
import { BrowserRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import App from './App';
import { initAppConfig, setBackendReachable } from './lib/config';
import { api } from './lib/api';
import { ensurePluginsLoaded, registerPlugin } from './plugins/registry';
import type { FrontendPlugin } from './plugins/types';
import './index.css';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      retry: 1,
    },
  },
});

// Runtime surface for out-of-process module chunks. Module bundles are built
// as IIFE with react / react/jsx-runtime / react-router-dom as externals —
// the globals below are the SAME instances the SPA uses, so router context
// and hooks work across the chunk boundary (a chunk that bundles its own
// react would crash on context crossing). registerPlugin at import time;
// api exposed for chunks that need HTTP access.
(window as unknown as Record<string, unknown>).__isolaPlugins = {
  registerPlugin: (plugin: FrontendPlugin) => registerPlugin(plugin),
};
(window as unknown as Record<string, unknown>).__isolaApi = api;
(window as unknown as Record<string, unknown>).React = React;
(window as unknown as Record<string, unknown>).ReactJSXRuntime = ReactJSXRuntime;
(window as unknown as Record<string, unknown>).ReactRouterDOM = ReactRouterDOM;

async function bootstrap() {
  let configPlugins: { name: string; config?: { chunk?: string } }[] = [];
  try {
    const res = await fetch('/api/config');
    const config = await res.json();
    initAppConfig(config);
    configPlugins = Array.isArray(config.plugins) ? config.plugins : [];
    setBackendReachable(true);
  } catch {
    setBackendReachable(false);
  }

  // Load module frontend chunks before first render so plugin routes and
  // toolbar buttons are present from the start.
  await ensurePluginsLoaded(configPlugins);

  ReactDOM.createRoot(document.getElementById('root')!).render(
    <React.StrictMode>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <App />
        </BrowserRouter>
      </QueryClientProvider>
    </React.StrictMode>,
  );
}

bootstrap();
