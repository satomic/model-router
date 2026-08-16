import { useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import { changeLocalPassword, setLocalAdminEnabled, type LocalAdminConfig } from '../../api'
import { formatDateTime } from '../../i18n/format'
import type { AccessSectionProps } from './types'

/** The local super administrator: the way into the console where github.com is not reachable.
 *
 *  Unlike its sibling sections this one does **not** feed the page's shared auth draft. A password
 *  must not sit in a form draft that another sub-page's Save could submit, so the enable toggle and
 *  the change-password form post to /v1/auth/local/* directly and take effect immediately. That is
 *  why `owns: []` in AccessPage -- there is nothing here for the dirty dot to track.
 */
export default function LocalAdminSection({ auth, notify }: AccessSectionProps) {
  const { t } = useTranslation()
  const local: LocalAdminConfig = auth.local_admin ?? {}

  // Mirrors the server after each successful write, since this section is not driven by the draft.
  const [enabled, setEnabled] = useState(local.enabled !== false)
  const [username, setUsername] = useState(local.username || 'admin')
  const [updatedAt, setUpdatedAt] = useState(local.updated_at ?? null)
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)

  const mismatch = Boolean(next && confirm && next !== confirm)
  const tooShort = Boolean(next) && next.length < 8
  const ready = Boolean(current && next && confirm) && !mismatch && !tooShort

  async function toggle(value: boolean) {
    setBusy(true)
    try {
      await setLocalAdminEnabled(value)
      setEnabled(value)
      notify('ok', value ? t('access.local.enabledSaved') : t('access.local.disabledSaved'))
    } catch (e) {
      notify('error', String(e))
    } finally {
      setBusy(false)
    }
  }

  async function submit() {
    setBusy(true)
    try {
      const res = await changeLocalPassword({
        current_password: current,
        new_password: next,
        new_username: username.trim() || undefined,
      })
      setUsername(res.username)
      setUpdatedAt(Date.now() / 1000)
      setCurrent('')
      setNext('')
      setConfirm('')
      notify('ok', t('access.local.passwordSaved'))
    } catch (e) {
      notify('error', String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <div className="panel">
        <div className="panel-head">
          {t('access.local.title')}
          <span className="spacer" />
          <span className={`badge ${enabled ? 'ok' : ''}`}>
            {enabled ? t('access.local.on') : t('access.local.off')}
          </span>
        </div>
        <div className="panel-body">
          <p className="panel-note">{t('access.local.lead')}</p>
          <label className="check">
            <input
              type="checkbox"
              checked={enabled}
              disabled={busy}
              onChange={(e) => void toggle(e.target.checked)}
            />
            {t('access.local.toggle')}
          </label>
          <p className="panel-note" style={{ marginTop: 10, marginBottom: 0 }}>
            <Trans i18nKey="access.local.note" components={{ strong: <strong />, code: <code /> }} />
          </p>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          {t('access.local.credentialTitle')}
          <span className="spacer" />
          <span className="dim">
            {updatedAt
              ? t('access.local.changedAt', { at: formatDateTime(updatedAt * 1000) })
              : t('access.local.neverChanged')}
          </span>
        </div>
        <div className="panel-body">
          <div className="row">
            <label className="field">
              <span className="field-name">
                {t('access.local.username')}
                <span className="field-hint">{t('access.local.usernameHint')}</span>
              </span>
              <input
                type="text"
                className="mono"
                value={username}
                disabled={busy}
                onChange={(e) => setUsername(e.target.value)}
              />
            </label>
            <label className="field">
              <span className="field-name">
                {t('access.local.current')}
                <span className="field-hint">{t('access.local.currentHint')}</span>
              </span>
              <input
                type="password"
                autoComplete="current-password"
                value={current}
                disabled={busy}
                onChange={(e) => setCurrent(e.target.value)}
              />
            </label>
          </div>
          <div className="row">
            <label className="field">
              <span className="field-name">
                {t('access.local.next')}
                <span className="field-hint">{t('access.local.nextHint')}</span>
              </span>
              <input
                type="password"
                autoComplete="new-password"
                value={next}
                disabled={busy}
                onChange={(e) => setNext(e.target.value)}
              />
            </label>
            <label className="field">
              <span className="field-name">{t('access.local.confirm')}</span>
              <input
                type="password"
                autoComplete="new-password"
                value={confirm}
                disabled={busy}
                onChange={(e) => setConfirm(e.target.value)}
              />
            </label>
          </div>
          {tooShort && <div className="toast warn">{t('password.tooShort')}</div>}
          {mismatch && <div className="toast warn">{t('password.mismatch')}</div>}
          <button className="btn" disabled={busy || !ready} onClick={() => void submit()}>
            {busy ? t('common.saving') : t('access.local.submit')}
          </button>
          <p className="panel-note" style={{ marginTop: 14, marginBottom: 0 }}>
            <Trans i18nKey="access.local.recovery" components={{ strong: <strong />, code: <code /> }} />
          </p>
        </div>
      </div>
    </>
  )
}
