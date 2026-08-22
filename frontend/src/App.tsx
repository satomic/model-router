import { useCallback, useEffect, useState, type ReactElement } from 'react'
import { useTranslation } from 'react-i18next'
import { Navigate, Route, Routes, useLocation, useNavigate, useSearchParams } from 'react-router-dom'
import Shell from './components/Shell'
import {
  getAuthStatus,
  getHealth,
  getReleaseStatus,
  RETURN_KEY,
  type AuthStatus,
  type Health,
  type ReleaseStatus,
} from './api'
import AccessPage from './pages/AccessPage'
import ChangePasswordPage from './pages/ChangePasswordPage'
import ConfigPage from './pages/ConfigPage'
import KeysPage from './pages/KeysPage'
import LoginPage from './pages/LoginPage'
import ModelsPage from './pages/ModelsPage'
import PlaygroundPage from './pages/PlaygroundPage'
import PolicyPage from './pages/PolicyPage'
import SetupPage from './pages/SetupPage'
import TracesPage from './pages/TracesPage'
import UsagePage from './pages/UsagePage'

type Page =
  | 'usage'
  | 'models'
  | 'keys'
  | 'traces'
  | 'config'
  | 'access'
  | 'policy'
  | 'playground'
type Theme = 'dark' | 'light'

/** The page '/' lands on. Also the fallback used while <Navigate> is settling. */
const HOME: Page = 'usage'

/**
 * Nav definition. A module-level const cannot call a hook, so it carries **translation
 * keys**; the labels/titles are resolved at render inside the component below. Each entry's
 * key doubles as its URL segment, which is what keeps the sidebar and the route table in
 * step without a mapping table.
 */
const NAV: {
  key: Page
  icon: string
  group: 'overview' | 'monitor' | 'manage'
  admin?: boolean
}[] = [
  { key: 'usage', icon: '▤', group: 'overview' },
  // Deliberately not admin-gated: the whole point of the model policy is that a regular user can
  // see their own curated list without asking an administrator what they were granted.
  { key: 'models', icon: '◈', group: 'overview' },
  { key: 'keys', icon: '⚿', group: 'overview' },
  { key: 'traces', icon: '☰', group: 'monitor' },
  { key: 'playground', icon: '▷', group: 'monitor' },
  { key: 'config', icon: '⚙', group: 'manage', admin: true },
  // A first-level page rather than a Routing configuration tab: it answers "which models may this
  // caller ask for", which is a different question from "which model serves this request", on
  // different config keys.
  { key: 'policy', icon: '◧', group: 'manage', admin: true },
  { key: 'access', icon: '⛨', group: 'manage', admin: true },
]

function useTheme(): [Theme, () => void] {
  const [theme, setTheme] = useState<Theme>(
    () => (localStorage.getItem('theme') as Theme | null) ?? 'light',
  )
  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
    localStorage.setItem('theme', theme)
  }, [theme])
  return [theme, () => setTheme((t) => (t === 'dark' ? 'light' : 'dark'))]
}

/** Shown for a path no route claims. The server cannot know the client's route list, so it
 *  hands over the shell and this is where an unknown URL actually lands. */
function NotFound() {
  const { t } = useTranslation()
  return <div className="empty">{t('app.notFound.lead')}</div>
}

/** An admin-only page reached by a non-admin -- a deep link that was shared too widely, most
 *  likely. The backend refuses the underlying calls regardless; this only avoids rendering a
 *  page whose every request would 403. */
function Forbidden() {
  const { t } = useTranslation()
  return <div className="empty">{t('app.forbidden.lead')}</div>
}

export default function App() {
  const { t, i18n } = useTranslation()
  const [status, setStatus] = useState<AuthStatus | null>(null)
  const [health, setHealth] = useState<Health | null>(null)
  const [rel, setRel] = useState<ReleaseStatus | null>(null)
  const [theme, toggleTheme] = useTheme()
  const navigate = useNavigate()
  const location = useLocation()
  const [params, setParams] = useSearchParams()

  const loginError = params.get('login_error')

  const refreshStatus = useCallback(
    () => getAuthStatus().then(setStatus).catch(() => setStatus(null)),
    [],
  )

  useEffect(() => {
    void refreshStatus()
  }, [refreshStatus])

  useEffect(() => {
    if (!status?.authenticated) return
    const load = () => getHealth().then(setHealth).catch(() => setHealth(null))
    load()
    const t = setInterval(load, 10000)
    return () => clearInterval(t)
  }, [status?.authenticated])

  // Once per sign-in, not on a timer: the answer comes from a cache the backend refreshes daily,
  // so polling it would ask the same question of the same local file over and over.
  useEffect(() => {
    if (!status?.authenticated) return
    getReleaseStatus().then(setRel).catch(() => setRel(null))
  }, [status?.authenticated])

  // Once signed in: go back to whatever page prompted the sign-in, and drop login_error from
  // the URL so it neither survives into the session nor travels in a copied link.
  useEffect(() => {
    if (!status?.authenticated) return
    const back = sessionStorage.getItem(RETURN_KEY)
    sessionStorage.removeItem(RETURN_KEY)
    if (back && back !== location.pathname + location.search) {
      navigate(back, { replace: true })
      return
    }
    if (params.has('login_error')) {
      const next = new URLSearchParams(params)
      next.delete('login_error')
      setParams(next, { replace: true })
    }
    // Deliberately keyed on the session alone: this is a one-shot on sign-in, not a rule that
    // should re-fire on every navigation.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status?.authenticated])

  if (status === null) {
    return <div className="auth-wrap"><div className="empty">{t('app.connecting')}</div></div>
  }
  // The wizard only preempts everything when there is **no** way in at all. With the local
  // super administrator enabled, an unreachable github.com must land on the sign-in card
  // instead of a wizard demanding OAuth credentials.
  if (!status.configured && !status.local_admin_enabled) {
    return <SetupPage status={status} onDone={refreshStatus} />
  }
  if (!status.authenticated || !status.user) {
    // Returning before <Routes> is what makes a signed-out deep link work with no guard and no
    // redirect: the URL is left alone, so signing in comes back to the very same page.
    return <LoginPage error={loginError} status={status} onSignedIn={refreshStatus} />
  }
  // Mirrors the server's gate: while the built-in default password is in force every endpoint
  // but status / logout / change-password is refused, so there is nothing else to render.
  if (status.user.must_change_password) {
    return <ChangePasswordPage user={status.user} onDone={refreshStatus} />
  }

  const user = status.user
  const nav = NAV.filter((n) => !n.admin || user.is_admin).map((n) => ({
    ...n,
    to: `/${n.key}`,
    label: t(`nav.${n.key}.label`),
    group: t(`nav.group.${n.group}`),
    title: t(`nav.${n.key}.title`),
    subtitle: t(`nav.${n.key}.subtitle`),
  }))

  // The URL is the single source of truth for which page is showing. `current` is undefined only
  // for a junk path, which falls back to the not-found heading. It is looked up in the unfiltered
  // NAV on purpose: an admin page viewed by a non-admin exists and is merely refused, so it must
  // keep its own heading rather than claim the address does not exist.
  const [seg0, seg1] = location.pathname.split('/').filter(Boolean)
  const key = seg0 ?? HOME
  const entry = NAV.find((n) => n.key === key)
  const current = entry && {
    title: t(`nav.${entry.key}.title`),
    subtitle: t(`nav.${entry.key}.subtitle`),
  }
  // Third breadcrumb level: a sub-page's own label, or a trace id shown verbatim.
  const sub = `${seg0}.section.${seg1}.label`
  const crumb =
    seg1 === undefined
      ? undefined
      : seg0 === 'traces'
        ? seg1
        : // i18n.exists rather than a defaultValue: i18next treats an empty-string default as no
          // default at all and hands back the key, so a junk sub-path such as /access/typo printed
          // "access.section.typo.label" into the breadcrumb instead of dropping the level.
          i18n.exists(sub)
          ? t(sub)
          : undefined

  /** Admin-only routes render their page or the refusal -- never a redirect, so the URL a user
   *  was given stays in the address bar and remains diagnosable. */
  const admin = (el: ReactElement) => (user.is_admin ? el : <Forbidden />)

  return (
    <Shell
      nav={nav}
      user={user}
      health={health}
      release={rel}
      theme={theme}
      onToggleTheme={toggleTheme}
      onLoggedOut={refreshStatus}
      title={current?.title ?? t('app.notFound.title')}
      subtitle={current?.subtitle}
      crumb={crumb}
    >
      <Routes>
        <Route path="/" element={<Navigate to={`/${HOME}`} replace />} />
        <Route path="/usage" element={<UsagePage user={user} />} />
        <Route path="/models" element={<ModelsPage />} />
        <Route path="/keys" element={<KeysPage user={user} />} />
        {/* Both trace routes render the same element, so opening and closing a detail view is a
            plain navigation rather than a remount. */}
        <Route path="/traces" element={<TracesPage user={user} />} />
        <Route path="/traces/:traceId" element={<TracesPage user={user} />} />
        <Route
          path="/playground"
          element={<PlaygroundPage onOpenTrace={(id) => navigate(`/traces/${id}`)} />}
        />
        <Route path="/config" element={admin(<Navigate to="/config/providers" replace />)} />
        {/* A :section change does not remount ConfigPage, so an unsaved draft survives a
            sub-page switch -- which nested <Outlet> routes would not give for free. */}
        <Route path="/config/:section" element={admin(<ConfigPage />)} />
        <Route path="/policy" element={admin(<PolicyPage />)} />
        <Route path="/access" element={admin(<Navigate to="/access/policy" replace />)} />
        <Route path="/access/:section" element={admin(<AccessPage />)} />
        <Route path="*" element={<NotFound />} />
      </Routes>
    </Shell>
  )
}
