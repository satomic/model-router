import { useEffect, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import type { AccessSectionProps } from './types'

// Accepts the full-width comma too, since a CJK keyboard produces it by default
const parseLogins = (text: string) =>
  text
    .split(/[,，\s]+/)
    .map((s) => s.trim())
    .filter(Boolean)

/** The administrator list and who may sign in. */
export default function AdminsSection({ auth, set }: AccessSectionProps) {
  const { t } = useTranslation()
  const admins = auth.admin_logins ?? []
  const openToAll = auth.allow_any_github_user !== false

  // Raw text lives here so separators being typed aren't normalized away mid-edit; the draft
  // (auth.admin_logins) only ever holds the parsed list.
  const [text, setText] = useState(admins.join(', '))
  useEffect(() => {
    // Resync only on external changes (load/revert), not on our own parse round-trips.
    if (JSON.stringify(parseLogins(text)) !== JSON.stringify(admins)) setText(admins.join(', '))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [admins.join('\u0000')])

  return (
    <>
      <div className="panel">
        <div className="panel-head">
          {t('access.admins.title')}
          <span className="badge admin">{t('access.admins.count', { count: admins.length })}</span>
        </div>
        <div className="panel-body">
          <label className="field" style={{ marginBottom: 0 }}>
            <span className="field-name">
              {t('access.admins.logins')}
              <span className="field-hint">{t('access.admins.loginsHint')}</span>
            </span>
            <input
              type="text"
              className="mono"
              value={text}
              placeholder="satomic, another-login"
              onChange={(e) => {
                setText(e.target.value)
                set({ admin_logins: parseLogins(e.target.value) })
              }}
            />
          </label>
          <p className="panel-note" style={{ marginTop: 12, marginBottom: 0 }}>
            <Trans
              i18nKey="access.admins.note"
              components={{ strong: <strong />, code: <code /> }}
            />
          </p>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">{t('access.signIn.title')}</div>
        <div className="panel-body">
          <label className="check">
            <input
              type="checkbox"
              checked={openToAll}
              onChange={(e) => set({ allow_any_github_user: e.target.checked })}
            />
            {t('access.signIn.allowAny')}
          </label>
          <p className="panel-note" style={{ marginTop: 10, marginBottom: 0 }}>
            <Trans
              i18nKey="access.signIn.note"
              components={{ strong: <strong />, code: <code /> }}
            />
          </p>
        </div>
      </div>
    </>
  )
}
