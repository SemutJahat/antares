import { lazy, type ComponentType, type LazyExoticComponent } from 'react'
import {
  ChartLineUp,
  ChatCircleDots,
  ClockCounterClockwise,
  Database,
  FileText,
  Fingerprint,
  Gear,
  GlobeHemisphereWest,
  HardDrives,
  Plugs,
  PlugsConnected,
  PuzzlePiece,
  ShareNetwork,
  UsersThree,
  Broadcast,
  Kanban,
  Robot,
  ShieldCheck,
  Sparkle,
  Terminal,
  Toolbox,
} from '@phosphor-icons/react'
import type { MessageKey } from './i18n'
import { ROUTE_MANIFEST, type RouteManifestEntry } from './routeManifest'

export type IconComponent = ComponentType<{
  className?: string
  weight?: 'regular' | 'fill' | 'bold' | 'duotone'
}>

export interface RouteDef extends RouteManifestEntry {
  icon: IconComponent
  component: LazyExoticComponent<ComponentType>
}

/**
 * React bits keyed by manifest id. Kept alongside the manifest so a new page
 * is two edits in this repo (manifest entry + this table); the shell reads
 * everything through the composed ROUTES list below.
 */
interface RouteRuntime {
  icon: IconComponent
  component: LazyExoticComponent<ComponentType>
}

const RUNTIME_BY_ID: Record<string, RouteRuntime> = {
  chat: {
    icon: ChatCircleDots,
    component: lazy(() => import('@/pages/ChatPage')),
  },
  sessions: {
    icon: ClockCounterClockwise,
    component: lazy(() => import('@/pages/SessionsPage')),
  },
  providers: {
    icon: Plugs,
    component: lazy(() => import('@/pages/ProvidersPage')),
  },
  tools: {
    icon: Toolbox,
    component: lazy(() => import('@/pages/ToolsPage')),
  },
  memory: {
    icon: Database,
    component: lazy(() => import('@/pages/MemoryPage')),
  },
  roles: {
    icon: UsersThree,
    component: lazy(() => import('@/pages/RolesPage')),
  },
  soul: {
    icon: Fingerprint,
    component: lazy(() => import('@/pages/SoulPage')),
  },
  skills: {
    icon: Sparkle,
    component: lazy(() => import('@/pages/SkillsPage')),
  },
  cron: {
    icon: ClockCounterClockwise,
    component: lazy(() => import('@/pages/CronPage')),
  },
  channels: {
    icon: Plugs,
    component: lazy(() => import('@/pages/ChannelsPage')),
  },
  autopilot: {
    icon: Robot,
    component: lazy(() => import('@/pages/AutopilotPage')),
  },
  engagement: {
    icon: ShieldCheck,
    component: lazy(() => import('@/pages/EngagementPage')),
  },
  board: {
    icon: Kanban,
    component: lazy(() => import('@/pages/BoardPage')),
  },
  intercept: {
    icon: Broadcast,
    component: lazy(() => import('@/pages/InterceptPage')),
  },
  mcp: {
    icon: PlugsConnected,
    component: lazy(() => import('@/pages/McpPage')),
  },
  plugins: {
    icon: PuzzlePiece,
    component: lazy(() => import('@/pages/PluginsPage')),
  },
  proxies: {
    icon: GlobeHemisphereWest,
    component: lazy(() => import('@/pages/ProxiesPage')),
  },
  vps: {
    icon: HardDrives,
    component: lazy(() => import('@/pages/VPSPage')),
  },
  analytics: {
    icon: ChartLineUp,
    component: lazy(() => import('@/pages/AnalyticsPage')),
  },
  files: {
    icon: FileText,
    component: lazy(() => import('@/pages/FilesPage')),
  },
  logs: {
    icon: Terminal,
    component: lazy(() => import('@/pages/LogsPage')),
  },
  config: {
    icon: Gear,
    component: lazy(() => import('@/pages/ConfigPage')),
  },
  system: {
    icon: Robot,
    component: lazy(() => import('@/pages/SystemPage')),
  },
  'social-media': {
    icon: ShareNetwork,
    component: lazy(() => import('@/pages/SocialMediaPage')),
  },
}

/**
 * The single source of truth for navigation, routing, and page chrome.
 * Pages render content only — the shell owns the container and header so every
 * screen shares identical spacing. Composed from {@link ROUTE_MANIFEST} so
 * build-time consumers (smoke, prerender) read the same list as the runtime.
 */
export const ROUTES: RouteDef[] = ROUTE_MANIFEST.map((entry) => {
  const runtime = RUNTIME_BY_ID[entry.id]
  if (!runtime) {
    throw new Error(`routes.ts: no runtime registered for manifest id "${entry.id}"`)
  }
  return { ...entry, icon: runtime.icon, component: runtime.component }
})

/** Nav entries use the short sidebar labels rather than the page titles. */
export const NAV_LABELS: Record<string, MessageKey> = {
  '/': 'nav.chat',
  '/sessions': 'nav.sessions',
  '/providers': 'nav.providers',
  '/tools': 'nav.tools',
  '/memory': 'nav.memory',
  '/skills': 'nav.skills',
  '/roles': 'nav.roles',
  '/soul': 'nav.soul',
  '/cron': 'nav.cron',
  '/channels': 'nav.channels',
  '/autopilot': 'nav.autopilot',
  '/engagement': 'nav.engagement',
  '/board': 'nav.board',
  '/intercept': 'nav.intercept',
  '/mcp': 'nav.mcp',
  '/plugins': 'nav.plugins',
  '/proxies': 'nav.proxies',
  '/vps': 'nav.vps',
  '/analytics': 'nav.analytics',
  '/files': 'nav.files',
  '/logs': 'nav.logs',
  '/config': 'nav.config',
  '/system': 'nav.system',
  '/social-media': 'nav.socialMedia',
}

export const PRIMARY_ROUTES = ROUTES.filter((r) => r.primary)

/** Resolve the route definition for a pathname. */
export function routeFor(pathname: string): RouteDef | undefined {
  const exact = ROUTES.find((r) => r.path === pathname)
  if (exact) return exact
  // Alias segments are matched by their static prefix (e.g. /c/<id>).
  return ROUTES.find((r) =>
    r.aliases?.some((a) => {
      const prefix = a.split('/:')[0]
      return prefix !== '' && pathname.startsWith(prefix + '/')
    }),
  )
}
