import type { FrontendPlugin, ToolbarButton, PluginRoute } from './types';
import type { ComponentType } from 'react';
import { useCallback, useEffect, useState } from 'react';

interface PluginProviderProps {
  sessionId: string;
  viewOnly: boolean;
  children: React.ReactNode;
}

const plugins: FrontendPlugin[] = [];

const listeners = new Set<() => void>();

function notify(): void {
  for (const l of listeners) l();
}

export function registerPlugin(plugin: FrontendPlugin): void {
  if (plugins.some((p) => p.info.name === plugin.info.name)) {
    return; // idempotent — chunks may be re-imported
  }
  plugins.push(plugin);
  notify();
}

export function getToolbarButtons(): ToolbarButton[] {
  return plugins
    .flatMap((p) => p.toolbarButtons || [])
    .sort((a, b) => a.order - b.order);
}

export function getPluginRoutes(): PluginRoute[] {
  return plugins.flatMap((p) => p.routes || []);
}

export function getPluginProviders(): ComponentType<PluginProviderProps>[] {
  return plugins
    .map((p) => p.provider)
    .filter((p): p is ComponentType<PluginProviderProps> => !!p);
}

// ---------------------------------------------------------------------------
// Runtime global for out-of-process module chunks. Module ESM bundles are
// built against this tiny surface (plus react, bundled inside the chunk) so
// they never import application modules directly. The SPA installs
// window.__isolaPlugins / window.__isolaApi at startup (see main.tsx).
// ---------------------------------------------------------------------------

interface IsolaChunkRuntime {
  registerPlugin: (plugin: FrontendPlugin) => void;
}

function chunkRuntime(): IsolaChunkRuntime | null {
  return (window as unknown as { __isolaPlugins?: IsolaChunkRuntime }).__isolaPlugins ?? null;
}

// ---------------------------------------------------------------------------
// Dynamic module loading. /api/config carries plugins[] manifests from the
// backend plugin registry; each entry with a chunk URL is imported once.
// ---------------------------------------------------------------------------

let pluginsLoaded = false;

export function ensurePluginsLoaded(
  configPlugins?: { name: string; config?: { chunk?: string } }[],
): Promise<void> {
  if (pluginsLoaded || !configPlugins?.length) return Promise.resolve();
  pluginsLoaded = true;
  const rt = chunkRuntime();
  if (!rt) return Promise.resolve(); // runtime not installed yet (very early import)

  return Promise.all(
    configPlugins
      .filter((p) => p.config?.chunk)
      .map(async (p) => {
        try {
          // Chunk self-registers via window.__isolaPlugins.registerPlugin.
          await import(/* @vite-ignore */ p.config!.chunk as string);
        } catch (err) {
          console.warn(`[plugin] chunk ${p.name}`, err);
        }
      }),
  ).then(() => undefined);
}

// usePlugins re-renders the caller whenever a plugin registers — used by
// App (routes) and ViewerPage (toolbar/providers) so async chunks appear
// without a full reload.
export function usePlugins(): number {
  const [version, setVersion] = useState(0);
  const bump = useCallback(() => setVersion((v) => v + 1), []);
  useEffect(() => {
    listeners.add(bump);
    return () => {
      listeners.delete(bump);
    };
  }, [bump]);
  return version;
}
