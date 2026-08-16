import { useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { githubLoginUrl, localLogin, RETURN_KEY, type AuthStatus } from '../api'
import LocalePicker from '../components/LocalePicker'

/** Signed-out state: an Azure-style sign-in card carrying whichever doors are actually open --
 *  GitHub OAuth when it is configured, the local super administrator when it is enabled. Both may
 *  be present; with only one, the other's markup is not rendered at all. */
export default function LoginPage({
  error,
  status,
  onSignedIn,
}: {
  error?: string | null
  status: AuthStatus
  onSignedIn: () => void
}) {
  const { t } = useTranslation()
  const [username, setUsername] = useState(status.local_admin_username || 'admin')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [localError, setLocalError] = useState('')

  // The local form starts collapsed behind a button, so GitHub is the one obvious way in. When
  // OAuth is *not* configured the local account is the only door there is, so hiding it behind an
  // extra click would be a dead end -- it opens expanded in that case.
  const [localOpen, setLocalOpen] = useState(!status.configured)
  const passwordRef = useRef<HTMLInputElement>(null)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setLocalError('')
    try {
      await localLogin(username, password)
      setPassword('')
      // App re-reads /v1/auth/status, which is what decides between the console and the
      // forced change-password form.
      onSignedIn()
    } catch {
      // One message for a wrong username and a wrong password alike -- the server does not
      // distinguish them and neither should the UI.
      setLocalError(t('login.local.failed'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="auth-wrap">
      <div className="auth-card">
        <div className="auth-lang"><LocalePicker compact /></div>
        <div className="logo">◆</div>
        <h1>{t('login.title')}</h1>
        <p className="lead">{t('login.lead')}</p>
        {error && <div className="toast error">{t('login.failed', { error })}</div>}
        {status.configured && (
          <a
            className="btn-github"
            href={githubLoginUrl}
            onClick={() => {
              // The OAuth callback can only redirect to a fixed path, so remember where the
              // user actually wanted to go and let App restore it once the session lands.
              const here = window.location.pathname + window.location.search
              if (here !== '/') sessionStorage.setItem(RETURN_KEY, here)
            }}
          >
            <svg width="18" height="18" viewBox="0 0 16 16" fill="currentColor" aria-hidden>
              <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.07-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.42 7.42 0 0 1 2-.27c.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A7.995 7.995 0 0 0 16 8c0-4.42-3.58-8-8-8Z" />
            </svg>
            {t('login.signInWithGitHub')}
          </a>
        )}
        {status.configured && status.local_admin_enabled && (
          <div className="auth-or"><span>{t('login.local.or')}</span></div>
        )}
        {status.local_admin_enabled && !localOpen && (
          <button
            className="btn-alt"
            type="button"
            aria-expanded={false}
            onClick={() => {
              setLocalOpen(true)
              // The username is prefilled, so the password is the field that actually wants the
              // caret. The rAF waits for the input to exist before focusing it.
              requestAnimationFrame(() => passwordRef.current?.focus())
            }}
          >
            {t('login.local.reveal')}
          </button>
        )}
        {status.local_admin_enabled && localOpen && (
          <form className="auth-form" onSubmit={submit}>
            <div className="field-name">{t('login.local.username')}</div>
            <input
              type="text"
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
            />
            <div className="field-name" style={{ marginTop: 8 }}>{t('login.local.password')}</div>
            <input
              ref={passwordRef}
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
            {localError && <div className="toast error" style={{ marginTop: 10 }}>{localError}</div>}
            <button className="btn primary" type="submit" disabled={busy || !username || !password}>
              {busy ? t('common.loading') : t('login.local.submit')}
            </button>
            {/* Collapsing again is only offered when there is something to go back to. */}
            {status.configured && (
              <button
                className="btn-link"
                type="button"
                style={{ marginTop: 10 }}
                onClick={() => { setLocalOpen(false); setPassword(''); setLocalError('') }}
              >
                {t('login.local.hide')}
              </button>
            )}
            <p className="faint" style={{ marginTop: 10 }}>{t('login.local.hint')}</p>
          </form>
        )}
      </div>
    </div>
  )
}
