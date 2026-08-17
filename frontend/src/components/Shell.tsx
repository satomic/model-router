import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { NavLink } from 'react-router-dom'
import { logout, type Health, type ReleaseStatus, type SessionUser } from '../api'
import LocalePicker from './LocalePicker'

/** Where the project lives, for the builds that predate /healthz reporting it. The server is the
 *  authority whenever it answers; these only keep the header's links working while it does not. */
const REPO_FALLBACK = 'https://github.com/satomic/model-router'
const ISSUES_FALLBACK = `${REPO_FALLBACK}/issues/new`
const RELEASES_FALLBACK = `${REPO_FALLBACK}/releases/latest`

export interface NavEntry {
  key: string
  /** Where this entry navigates to, e.g. '/config'. A real href, so the item is
   *  middle-clickable, copyable and bookmarkable. */
  to: string
  label: string
  icon: string
  group: string
}

/**
 * Azure-portal-style shell: dark blue header on top, a collapsible left nav, and a
 * breadcrumb/command bar above the content area.
 */
export default function Shell({
  nav,
  user,
  health,
  release,
  theme,
  onToggleTheme,
  onLoggedOut,
  title,
  subtitle,
  crumb,
  commands,
  children,
}: {
  nav: NavEntry[]
  user: SessionUser
  health: Health | null
  /** The last answer from the background release check, or null while it has not been read. */
  release?: ReleaseStatus | null
  theme: 'light' | 'dark'
  onToggleTheme: () => void
  onLoggedOut: () => void
  title: string
  subtitle?: string
  /** Optional third breadcrumb level, for pages that have sub-pages of their own. */
  crumb?: string
  commands?: ReactNode
  children: ReactNode
}) {
  const { t } = useTranslation()
  const [collapsed, setCollapsed] = useState(false)
  const [menuOpen, setMenuOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!menuOpen) return
    const onDown = (e: MouseEvent) => {
      if (!menuRef.current?.contains(e.target as Node)) setMenuOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [menuOpen])

  const groups: string[] = []
  for (const n of nav) if (!groups.includes(n.group)) groups.push(n.group)

  const initials = (user.name || user.login).slice(0, 2).toUpperCase()

  // Server-reported where available. The version has no fallback on purpose: inventing one would
  // be a claim about which build is running, and an absent version is more honest than a wrong one.
  const version = health?.version
  const repoUrl = health?.repo_url || REPO_FALLBACK
  const issuesUrl = health?.issues_url || ISSUES_FALLBACK
  const releasesUrl = release?.release_url || health?.releases_url || RELEASES_FALLBACK
  const updateAvailable = Boolean(release?.update_available && release.latest_version)

  return (
    <>
      <header className="topbar">
        <button
          className="hamburger"
          title={collapsed ? t('shell.expandNav') : t('shell.collapseNav')}
          onClick={() => setCollapsed((c) => !c)}
        >
          ☰
        </button>
        <span className="brand">
          <span className="glyph">◆</span> Model Router
          {version && <span className="version">v{version}</span>}
        </span>
        <div className="status">
          {updateAvailable && (
            /* Shown only when a newer tag exists, and it links straight to that release rather
               than to the repository: an update notice that lands on a project page still leaves
               the reader hunting for what changed. */
            <a
              className="chip update"
              href={releasesUrl}
              target="_blank"
              rel="noreferrer"
              title={t('shell.update.title', { version: release!.latest_version })}
            >
              <span aria-hidden>↑</span>{' '}
              {t('shell.update.available', { version: release!.latest_version })}
            </a>
          )}
          <a
            className="icon-btn"
            href={repoUrl}
            target="_blank"
            rel="noreferrer"
            title={t('shell.sourceRepo')}
            aria-label={t('shell.sourceRepo')}
          >
            {/* The GitHub mark, inline: the console loads no third-party assets, so an icon font
                or a remote image would be the only such request in the app. */}
            <svg viewBox="0 0 16 16" width="15" height="15" fill="currentColor" aria-hidden>
              <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.07-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.42 7.42 0 0 1 2-.27c.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A7.995 7.995 0 0 0 16 8c0-4.42-3.58-8-8-8Z" />
            </svg>
          </a>
          <a
            className="icon-btn"
            href={issuesUrl}
            target="_blank"
            rel="noreferrer"
            title={t('shell.reportIssue')}
            aria-label={t('shell.reportIssue')}
          >
            ⚑
          </a>
          {health ? (
            <span className="chip">
              <span className="dot">●</span> {t('shell.running')} · {health.strategy}
              {health.sticky ? ' · sticky' : ''}
            </span>
          ) : (
            <span className="chip">
              <span className="dot off">●</span> {t('shell.unreachable')}
            </span>
          )}
          <LocalePicker />
          <button className="icon-btn" onClick={onToggleTheme} title={t('shell.toggleTheme')}>
            {theme === 'dark' ? '☾' : '☀'}
          </button>
          <div className="user-menu" ref={menuRef}>
            <button className="user-pill" onClick={() => setMenuOpen((o) => !o)}>
              <span className="avatar">
                {user.avatar_url ? <img src={user.avatar_url} alt="" /> : initials}
              </span>
              {/* The display name, falling back to the login when GitHub has none set (and for the
                  local administrator, which has no name at all). The dropdown below still shows
                  the login underneath, so the account is never ambiguous. */}
              <span>{user.name || user.login}</span>
              <span aria-hidden>▾</span>
            </button>
            {menuOpen && (
              <div className="menu-pop">
                <div className="who">
                  <span className="avatar">
                    {user.avatar_url ? <img src={user.avatar_url} alt="" /> : initials}
                  </span>
                  <div>
                    <div className="name">{user.name || user.login}</div>
                    <div className="dim mono" style={{ fontSize: 11.5 }}>{user.login}</div>
                  </div>
                </div>
                <div style={{ marginBottom: 12 }}>
                  <span className={`badge ${user.is_admin ? 'admin' : ''}`}>
                    {user.is_admin ? t('shell.roleAdmin') : t('shell.roleUser')}
                  </span>
                </div>
                <button
                  className="btn ghost sm"
                  style={{ width: '100%' }}
                  onClick={async () => {
                    try {
                      await logout()
                    } finally {
                      onLoggedOut()
                    }
                  }}
                >
                  {t('shell.signOut')}
                </button>
              </div>
            )}
          </div>
        </div>
      </header>

      <div className="shell">
        <nav className={`sidenav ${collapsed ? 'collapsed' : ''}`}>
          {groups.map((g) => (
            <div key={g}>
              <div className="nav-group">{g}</div>
              {nav
                .filter((n) => n.group === g)
                .map((n) => (
                  // `end` is deliberately omitted: /config must stay highlighted while the
                  // URL is /config/models.
                  <NavLink
                    key={n.key}
                    to={n.to}
                    className={({ isActive }) => `nav-item ${isActive ? 'active' : ''}`}
                    title={n.label}
                  >
                    <span className="ico" aria-hidden>{n.icon}</span>
                    <span className="label">{n.label}</span>
                  </NavLink>
                ))}
            </div>
          ))}
        </nav>

        <div className="content">
          <div className="breadcrumb">
            <span>Model Router</span>
            <span className="sep">›</span>
            <span>{title}</span>
            {crumb && (
              <>
                <span className="sep">›</span>
                <span>{crumb}</span>
              </>
            )}
          </div>
          <div className="page-title">
            <div>
              <h1>{title}</h1>
              {subtitle && <div className="sub">{subtitle}</div>}
            </div>
          </div>
          {commands && <div className="cmdbar">{commands}</div>}
          <main className="page">{children}</main>
        </div>
      </div>
    </>
  )
}
