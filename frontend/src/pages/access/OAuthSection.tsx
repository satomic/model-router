import { useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import type { AccessSectionProps } from './types'

/** The GitHub OAuth application credentials, configurable and updatable from the UI.
 *
 *  Security considerations: the secret uses a password input and is masked by default; getting
 *  it wrong locks everybody out of signing in, so there is an explicit warning at the top, and
 *  leaving "callback URL" empty derives it from the request origin (the hardest thing to get
 *  wrong).
 */
export default function OAuthSection({ auth, set, saved }: AccessSectionProps) {
  const { t } = useTranslation()
  const [reveal, setReveal] = useState(false)
  const gh = auth.github ?? {}
  const savedGh = saved.github ?? {}
  const configured = Boolean(savedGh.client_id && savedGh.client_secret)
  const origin = window.location.origin

  const setGh = (patch: Partial<typeof gh>) => set({ github: { ...gh, ...patch } })

  return (
    <>
      <div className="panel">
        <div className="panel-head">
          {t('access.oauth.title')}
          {configured ? (
            <span className="badge ok">{t('access.oauth.configured')}</span>
          ) : (
            <span className="badge error">{t('access.oauth.notConfigured')}</span>
          )}
        </div>
        <div className="panel-body">
          <p className="panel-note" style={{ marginTop: 0 }}>
            <Trans
              i18nKey="access.oauth.warning"
              components={{ strong: <strong />, code: <code /> }}
            />
          </p>

          <label className="field">
            <span className="field-name">
              Client ID
              <span className="field-hint">{t('access.oauth.clientIdHint')}</span>
            </span>
            <input
              type="text"
              className="mono"
              value={gh.client_id ?? ''}
              placeholder="Ov23li..."
              onChange={(e) => setGh({ client_id: e.target.value.trim() })}
            />
          </label>

          <label className="field">
            <span className="field-name">
              Client Secret
              <span className="field-hint">{t('access.oauth.clientSecretHint')}</span>
            </span>
            <div className="secret-reveal">
              <input
                type={reveal ? 'text' : 'password'}
                className="mono"
                value={gh.client_secret ?? ''}
                placeholder={
                  configured
                    ? t('access.oauth.secretKeepPlaceholder')
                    : t('access.oauth.secretPastePlaceholder')
                }
                onChange={(e) => setGh({ client_secret: e.target.value.trim() })}
              />
              <button className="btn ghost sm" onClick={() => setReveal((v) => !v)}>
                {reveal ? t('common.hide') : t('common.show')}
              </button>
            </div>
          </label>

          <label className="field" style={{ marginBottom: 0 }}>
            <span className="field-name">
              {t('access.oauth.callbackUrl')}
              <span className="field-hint">{t('access.oauth.callbackUrlHint')}</span>
            </span>
            <input
              type="text"
              className="mono"
              value={gh.callback_url ?? ''}
              placeholder={`${origin}/v1/auth/github/callback`}
              onChange={(e) => setGh({ callback_url: e.target.value.trim() })}
            />
          </label>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">{t('access.oauth.registerTitle')}</div>
        <div className="panel-body">
          <p className="panel-note" style={{ marginTop: 0 }}>
            {t('access.oauth.registerNote')}
          </p>
          <dl className="kv">
            <dt>Homepage URL</dt>
            <dd className="mono">{origin}/</dd>
            <dt>Authorization callback URL</dt>
            <dd className="mono">{gh.callback_url || `${origin}/v1/auth/github/callback`}</dd>
          </dl>
        </div>
      </div>
    </>
  )
}
