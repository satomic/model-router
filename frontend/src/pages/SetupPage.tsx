import { useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import { setupAuth, type AuthStatus } from '../api'
import LocalePicker from '../components/LocalePicker'

/**
 * First-run wizard: writes the GitHub OAuth credentials and the administrator list.
 * The backend only accepts this from the local machine.
 */
export default function SetupPage({
  status,
  onDone,
}: {
  status: AuthStatus
  onDone: () => void
}) {
  const { t } = useTranslation()
  const [clientId, setClientId] = useState('')
  const [clientSecret, setClientSecret] = useState('')
  const [admins, setAdmins] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const callback = status.callback_url

  const submit = async () => {
    setBusy(true)
    setError('')
    try {
      await setupAuth({
        client_id: clientId.trim(),
        client_secret: clientSecret.trim(),
        // Accepts the full-width comma too, since a CJK keyboard produces it by default
        admin_logins: admins.split(/[,，\s]+/).map((s) => s.trim()).filter(Boolean),
      })
      onDone()
    } catch (e) {
      setError(String(e))
    } finally {
      setBusy(false)
    }
  }

  if (!status.can_setup) {
    return (
      <div className="auth-wrap">
        <div className="auth-card wide">
          <div className="auth-lang"><LocalePicker compact /></div>
          <div className="logo">◆</div>
          <h1>{t('setup.blocked.title')}</h1>
          <p className="lead">
            <Trans i18nKey="setup.blocked.lead" components={{ code: <code /> }} />
          </p>
          <pre className="code">{`auth:
  github:
    client_id: 'Iv1.xxxxxxxx'
    client_secret: 'xxxxxxxx'
    callback_url: ''      # ${t('setup.blocked.callbackComment')}
  admin_logins: [your-github-login]
  allow_any_github_user: true`}</pre>
          {/* This wizard is only reached when the local administrator is off too, so where GitHub
              is unreachable it is the only remaining way in and belongs on this page. */}
          <p className="panel-note" style={{ textAlign: 'left' }}>
            <Trans i18nKey="setup.localHint" components={{ code: <code /> }} />
          </p>
        </div>
      </div>
    )
  }

  return (
    <div className="auth-wrap">
      <div className="auth-card wide">
        <div className="auth-lang"><LocalePicker compact /></div>
        <div className="logo">◆</div>
        <h1>{t('setup.title')}</h1>
        <p className="lead">{t('setup.lead')}</p>

        <div className="toast info">
          <div>
            <div style={{ fontWeight: 600, marginBottom: 2 }}>Authorization callback URL</div>
            <code className="mono">{callback}</code>
          </div>
        </div>

        {error && <div className="toast error">{error}</div>}

        <label className="field">
          <span className="field-name">Client ID</span>
          <input
            type="text"
            className="mono"
            value={clientId}
            onChange={(e) => setClientId(e.target.value)}
            placeholder="Iv1.xxxxxxxxxxxx"
          />
        </label>
        <label className="field">
          <span className="field-name">Client Secret</span>
          <input
            type="password"
            className="mono"
            value={clientSecret}
            onChange={(e) => setClientSecret(e.target.value)}
            placeholder={t('setup.secretPlaceholder')}
          />
        </label>
        <label className="field">
          <span className="field-name">
            {t('setup.adminLogins')}
            <span className="field-hint">{t('setup.adminLoginsHint')}</span>
          </span>
          <input
            type="text"
            className="mono"
            value={admins}
            onChange={(e) => setAdmins(e.target.value)}
            placeholder="satomic"
          />
        </label>

        <button
          className="btn"
          style={{ width: '100%' }}
          disabled={busy || !clientId.trim() || !clientSecret.trim() || !admins.trim()}
          onClick={submit}
        >
          {busy ? t('setup.saving') : t('setup.save')}
        </button>
        <p className="panel-note" style={{ textAlign: 'left', marginTop: 14, marginBottom: 0 }}>
          <Trans i18nKey="setup.localHint" components={{ code: <code /> }} />
        </p>
      </div>
    </div>
  )
}
