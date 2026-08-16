import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { NavLink } from 'react-router-dom'
import { logout, type SessionUser } from '../api'
import LocalePicker from './LocalePicker'

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
  health: { strategy: string; sticky: boolean } | null
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
        </span>
        <div className="status">
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
