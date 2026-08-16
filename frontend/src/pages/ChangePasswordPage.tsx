import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { changeLocalPassword, logout, type SessionUser } from '../api'
import LocalePicker from '../components/LocalePicker'

/** The forced change-password form, rendered *before* the shell whenever the session still runs
 *  on the built-in default credential. The server refuses every endpoint but status / logout /
 *  this one, so the client is mirroring a gate it does not enforce -- there is nothing to reach by
 *  editing the URL. */
export default function ChangePasswordPage({
  user,
  onDone,
}: {
  user: SessionUser
  onDone: () => void
}) {
  const { t } = useTranslation()
  const [username, setUsername] = useState(user.login)
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  // Checked here only to spare a round trip; length and the "not the default" rule are enforced
  // server-side regardless.
  const mismatch = Boolean(next && confirm && next !== confirm)
  const tooShort = Boolean(next) && next.length < 8
  const ready = Boolean(current && next && confirm) && !mismatch && !tooShort

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await changeLocalPassword({
        current_password: current,
        new_password: next,
        new_username: username.trim() || undefined,
      })
      onDone()
    } catch (err) {
      setError(String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="auth-wrap">
      <div className="auth-card">
        <div className="auth-lang"><LocalePicker compact /></div>
        <div className="logo">◆</div>
        <h1>{t('password.title')}</h1>
        <p className="lead">{t('password.lead')}</p>
        <form className="auth-form" onSubmit={submit}>
          <div className="field-name">{t('password.username')}</div>
          <input type="text" value={username} onChange={(e) => setUsername(e.target.value)} />
          <div className="field-name" style={{ marginTop: 8 }}>{t('password.current')}</div>
          <input
            type="password"
            autoComplete="current-password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
          <div className="field-name" style={{ marginTop: 8 }}>{t('password.next')}</div>
          <input
            type="password"
            autoComplete="new-password"
            value={next}
            onChange={(e) => setNext(e.target.value)}
          />
          <div className="field-name" style={{ marginTop: 8 }}>{t('password.confirm')}</div>
          <input
            type="password"
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
          {tooShort && <div className="toast warn" style={{ marginTop: 10 }}>{t('password.tooShort')}</div>}
          {mismatch && <div className="toast warn" style={{ marginTop: 10 }}>{t('password.mismatch')}</div>}
          {error && <div className="toast error" style={{ marginTop: 10 }}>{error}</div>}
          <button className="btn primary" type="submit" disabled={busy || !ready}>
            {busy ? t('common.loading') : t('password.submit')}
          </button>
        </form>
        <p className="faint" style={{ marginTop: 12 }}>
          {t('password.recovery')}
        </p>
        <button className="btn-link" style={{ marginTop: 8 }} onClick={() => logout().then(onDone)}>
          {t('shell.signOut')}
        </button>
      </div>
    </div>
  )
}
