import React from 'react';
import { Routes, Route, Navigate } from 'react-router-dom';
import LandingPage from './pages/LandingPage';

// Route-level chunks: the landing page must not download the viewer, the
// Analysis panel or the captures/network pages. LandingPage stays eager so the
// first paint has no extra round trip.
const ViewerPage = React.lazy(() => import('./pages/ViewerPage'));
const NetworkPage = React.lazy(() => import('./pages/NetworkPage'));
const CapturesPage = React.lazy(() => import('./pages/CapturesPage'));
const RunPage = React.lazy(() => import('./pages/RunPage'));
const SharePage = React.lazy(() => import('./pages/SharePage'));
import { ThemeProvider } from './lib/ThemeContext';
import { LoadingOverlay } from './components/LoadingOverlay';
import { useLaunchProgress } from './lib/launchProgress';
import { getPluginRoutes, usePlugins } from './plugins/registry';

function LazyFallback() {
  return <div className="flex items-center justify-center min-h-screen bg-zinc-200 dark:bg-gray-950" />;
}

// Lives above the router so the launch overlay is never remounted when
// RunPage hands the freshly created session over to ViewerPage.
function LaunchOverlayHost() {
  const launch = useLaunchProgress();
  if (!launch.active) return null;
  return (
    <div className="fixed inset-0 z-[60]">
      <LoadingOverlay status={launch.status} isReconnecting={false} caption={launch.caption} />
    </div>
  );
}

export default function App() {
  const pluginVersion = usePlugins();
  const pluginRoutes = React.useMemo(() => {
    void pluginVersion;
    return getPluginRoutes();
  }, [pluginVersion]);

  return (
    <ThemeProvider>
      <React.Suspense fallback={<LazyFallback />}>
        <Routes>
          <Route path="/" element={<LandingPage />} />
          {pluginRoutes.map((r) => (
            <Route key={r.path} path={r.path} element={r.element} />
          ))}
          {/* Static segments outrank the /session/:id viewer route; order here
              does not matter to the router, only to the reader. */}
          <Route path="/session/:id/network" element={<NetworkPage />} />
          <Route path="/session/:id/captures" element={<CapturesPage />} />
          <Route path="/captures/:id" element={<CapturesPage />} />
          <Route path="/session/:id" element={<ViewerPage />} />
          <Route path="/run" element={<RunPage />} />
          <Route path="/share/:token" element={<SharePage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
        <LaunchOverlayHost />
      </React.Suspense>
    </ThemeProvider>
  );
}
