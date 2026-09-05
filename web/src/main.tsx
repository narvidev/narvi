import type { ComponentType } from 'react'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createRouter } from '@tanstack/react-router'

import { initTheme } from './lib/theme'
import { installUnauthorizedHandler } from './auth/session'
import { RouteErrorFallback, RouteNotFoundFallback } from './components/shell/RouteFallbacks'
import { routeTree } from './routeTree.gen'
import { registerSlot, type SlotId } from './ext/slots'
import './styles/tokens.css'
import './styles/base.css'

// createAppRouter builds the SAME router bootstrap() renders below --
// extracted to a named function (rather than a module-level const) purely
// so the `declare module` registration a few lines down can reference its
// return type without needing a real QueryClient at import time.
function createAppRouter(queryClient: QueryClient) {
  // defaultErrorComponent/defaultNotFoundComponent are wired here rather
  // than per-route: they are the shell's own terminal states, and leaving
  // them unset is what left the app rendering TanStack's bare defaults --
  // an unstyled exception dump on any backend hiccup, and a
  // corner-of-the-page "Not Found". See components/shell/RouteFallbacks.tsx
  // for why the error one never renders the underlying message.
  return createRouter({
    routeTree,
    context: { queryClient },
    defaultErrorComponent: RouteErrorFallback,
    defaultNotFoundComponent: RouteNotFoundFallback,
  })
}

// Registers createAppRouter's own route/param types with TanStack
// Router's global type registry, exactly as its own setup docs require --
// without this, every <Link to="..."> across the app loses type-checked
// route paths.
declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}

/**
 * bootstrap builds the QueryClient, router and providers exactly as this
 * file always has, and mounts them at #root -- extracted into an
 * exported function (docs/design/boundaries-design.md, section 4.5, item
 * 2) so a future private Gatekeeper bundle can call it with its own
 * `slots` instead of forking or re-implementing this file: `bootstrap({})`
 * below is this repository's own call, registering nothing (see ext/
 * slots.tsx's own doc comment for why the public bundle's slot registry
 * stays empty forever).
 *
 * options.slots is a partial map of SlotId -> component, registered via
 * ext/slots.tsx's own registerSlot before the router is built -- so a
 * slot a private caller supplies is already present the first time any
 * route renders a SlotOutlet for it. Nothing in this repository ever
 * passes a non-empty map.
 */
export function bootstrap(options: { slots?: Partial<Record<SlotId, ComponentType<Record<string, never>>>> } = {}): void {
  // Re-applies the SAME preference index.html's own inline pre-paint
  // script already set on <html> -- see theme.ts's own doc comment for
  // why this is harmless-but-correct redundancy rather than dead code.
  initTheme()

  for (const [id, component] of Object.entries(options.slots ?? {}) as [SlotId, ComponentType<Record<string, never>>][]) {
    registerSlot(id, component)
  }

  // §12.1's data-layer client pattern ("WS transport -> event log ->
  // reducer -> query invalidation") starts here with the query cache; the
  // WS transport and reducer land with the real data layer, not this
  // bootstrap -- no queries are defined yet.
  const queryClient = new QueryClient()

  // §13.1: wires http.ts's generic 401 hook to THIS app's real
  // consequence (invalidate the cached "who am I" query) -- see
  // installUnauthorizedHandler's own doc comment for why this belongs
  // here, once, rather than per-component.
  installUnauthorizedHandler(queryClient)

  const router = createAppRouter(queryClient)

  const rootElement = document.getElementById('root')
  if (!rootElement) {
    throw new Error('main.tsx: #root element missing from index.html')
  }

  createRoot(rootElement).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </StrictMode>,
  )
}

bootstrap({})
