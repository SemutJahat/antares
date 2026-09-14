import type { MessageKey } from './i18n'

/**
 * Pure-data route manifest. No React, no icons, no lazy components — safe to
 * import from build scripts, smoke tests, or any pre-render tool that just
 * needs to know which paths exist and how the shell should frame them.
 *
 * Runtime routing (icons + lazy components) is composed in {@link ./routes.ts}
 * by mapping this list. Keep both files in sync by editing the manifest here;
 * routes.ts merely attaches the React bits by page id.
 */
export interface RouteManifestEntry {
  /** Stable page id: routes.ts joins on this to attach icon + component. */
  id: string
  /** Primary pathname. */
  path: string
  /** Extra paths that render the same page (e.g. /c/:id for chat). */
  aliases?: string[]
  titleKey: MessageKey
  descKey?: MessageKey
  /** Shown in the mobile bottom bar. */
  primary?: boolean
  /** Renders without the standard page container and header. */
  fullBleed?: boolean
  /**
   * Opt out of the default fixed-height frame: let this page size to its
   * content and scroll with the document instead of scrolling inside a fixed
   * container. For genuinely short, static pages.
   */
  staticHeight?: boolean
}

export const ROUTE_MANIFEST: RouteManifestEntry[] = [
  {
    id: 'chat',
    path: '/',
    aliases: ['/c/:sessionId'],
    titleKey: 'nav.chat',
    primary: true,
    fullBleed: true,
  },
  {
    id: 'sessions',
    path: '/sessions',
    titleKey: 'sessions.title',
    descKey: 'sessions.desc',
    primary: true,
  },
  {
    id: 'providers',
    path: '/providers',
    aliases: ['/models'],
    titleKey: 'providers.title',
    descKey: 'providers.desc',
    primary: true,
  },
  {
    id: 'tools',
    path: '/tools',
    titleKey: 'tools.title',
    descKey: 'tools.desc',
  },
  {
    id: 'memory',
    path: '/memory',
    titleKey: 'memory.title',
    descKey: 'memory.desc',
  },
  {
    id: 'roles',
    path: '/roles',
    titleKey: 'roles.title',
    descKey: 'roles.desc',
  },
  {
    id: 'soul',
    path: '/soul',
    titleKey: 'soul.title',
    descKey: 'soul.desc',
    staticHeight: true,
  },
  {
    id: 'skills',
    path: '/skills',
    titleKey: 'skills.title',
    descKey: 'skills.desc',
  },
  {
    id: 'cron',
    path: '/cron',
    titleKey: 'cron.title',
    descKey: 'cron.desc',
  },
  {
    id: 'channels',
    path: '/channels',
    titleKey: 'channels.title',
    descKey: 'channels.desc',
  },
  {
    id: 'autopilot',
    path: '/autopilot',
    titleKey: 'autopilot.title',
    descKey: 'autopilot.desc',
  },
  {
    id: 'engagement',
    path: '/engagement',
    titleKey: 'engagement.title',
    descKey: 'engagement.desc',
  },
  {
    id: 'board',
    path: '/board',
    titleKey: 'board.title',
    descKey: 'board.desc',
  },
  {
    id: 'intercept',
    path: '/intercept',
    titleKey: 'intercept.title',
    descKey: 'intercept.desc',
  },
  {
    id: 'mcp',
    path: '/mcp',
    titleKey: 'mcp.title',
    descKey: 'mcp.desc',
  },
  {
    id: 'plugins',
    path: '/plugins',
    titleKey: 'plugins.title',
    descKey: 'plugins.desc',
  },
  {
    id: 'proxies',
    path: '/proxies',
    titleKey: 'proxies.title',
    descKey: 'proxies.desc',
  },
  {
    id: 'vps',
    path: '/vps',
    titleKey: 'vps.title',
    descKey: 'vps.desc',
  },
  {
    id: 'analytics',
    path: '/analytics',
    titleKey: 'analytics.title',
    descKey: 'analytics.desc',
  },
  {
    id: 'files',
    path: '/files',
    titleKey: 'files.title',
    descKey: 'files.desc',
  },
  {
    id: 'logs',
    path: '/logs',
    titleKey: 'logs.title',
    descKey: 'logs.desc',
  },
  {
    id: 'config',
    path: '/config',
    titleKey: 'config.title',
    descKey: 'config.desc',
    primary: true,
  },
  {
    id: 'system',
    path: '/system',
    titleKey: 'system.title',
    descKey: 'system.desc',
  },
  {
    id: 'social-media',
    path: '/social-media',
    titleKey: 'social.title',
    descKey: 'social.desc',
  },
]

/**
 * Fixture segment substituted for alias parameters when producing smoke
 * targets. Stable so recorded runs diff cleanly across invocations.
 */
export const SMOKE_FIXTURE_SESSION_ID = 'smoke-session'

const SMOKE_FIXTURES: Record<string, string> = {
  sessionId: SMOKE_FIXTURE_SESSION_ID,
}

function materializePath(pattern: string): string {
  return pattern.replace(/:([A-Za-z0-9_]+)/g, (_, name: string) => {
    const value = SMOKE_FIXTURES[name]
    // Unknown parameter → fall back to the fixture id rather than leaving a
    // literal ":param" segment behind; smoke callers can override by extending
    // SMOKE_FIXTURES if they add a new param.
    return value ?? SMOKE_FIXTURE_SESSION_ID
  })
}

/**
 * Concrete pathnames a headless smoke run should visit: every canonical route,
 * every alias with parameters filled from fixtures, plus the standalone
 * onboarding surfaces that live outside the manifest.
 */
export function smokeTargets(): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  const push = (p: string) => {
    if (seen.has(p)) return
    seen.add(p)
    out.push(p)
  }
  push('/setup')
  push('/login')
  for (const entry of ROUTE_MANIFEST) {
    push(materializePath(entry.path))
    for (const alias of entry.aliases ?? []) push(materializePath(alias))
  }
  return out
}
