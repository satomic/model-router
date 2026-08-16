import { Trans, useTranslation } from 'react-i18next'
import type { ModelMeta } from '../../api'
import type { SectionProps } from './types'

/** Model catalog: the names exposed to clients, the connection each is bound to, and the
 *  descriptions the AI decision model reads. */
export default function ModelsSection({ cfg, set, notify, goto }: SectionProps) {
  const { t } = useTranslation()
  const providerNames = Object.keys(cfg.providers ?? {})
  const modelNames = Object.keys(cfg.models)

  const setModel = (name: string, patch: Partial<ModelMeta>) =>
    set({ models: { ...cfg.models, [name]: { ...cfg.models[name], ...patch } } })

  const setDefault = (name: string) => {
    const models = Object.fromEntries(
      Object.entries(cfg.models).map(([k, v]) => {
        const { default: _omit, ...rest } = v
        return [k, k === name ? { ...rest, default: true } : rest]
      }),
    )
    set({ models })
  }

  const addModel = () => {
    const name = prompt(t('config.models.promptName'))?.trim()
    if (!name) return
    if (cfg.models[name]) {
      notify('error', t('config.models.alreadyExists', { name }))
      return
    }
    set({ models: { ...cfg.models, [name]: { description: '' } } })
  }

  const removeModel = (name: string) => {
    const affected = cfg.rules.filter((r) => r.model === name)
    const { [name]: _omit, ...rest } = cfg.models
    set({ models: rest, rules: cfg.rules.filter((r) => r.model !== name) })
    if (affected.length) {
      notify('ok', t('config.models.removedWithRules', { name, count: affected.length }))
    }
  }

  return (
    <>
      <div className="panel">
        <div className="panel-head">
          {t('config.models.title')}
          <span className="badge">{t('config.models.count', { count: modelNames.length })}</span>
          <span className="spacer" />
          <button className="btn ghost sm" onClick={addModel}>{t('config.models.add')}</button>
        </div>
        <div className="panel-body">
          <p className="panel-note" style={{ marginBottom: 0 }}>
            <Trans
              i18nKey="config.models.lead"
              components={{
                providersLink: (
                  <button className="btn subtle sm" onClick={() => goto('providers')} />
                ),
              }}
            />
          </p>
        </div>
      </div>

      {modelNames.length === 0 && (
        <div className="panel">
          <div className="empty">{t('config.models.empty')}</div>
        </div>
      )}

      {modelNames.map((name) => {
        const m = cfg.models[name]
        const bound = m.provider ?? cfg.default_provider
        const missing = !cfg.providers?.[bound]
        return (
          <div className="model-card" key={name}>
            <div className="head">
              <span className="name">{name}</span>
              {m.default && <span className="badge ok">{t('config.models.defaultBadge')}</span>}
              {m.reasoning && <span className="badge warn">{t('config.models.reasoningBadge')}</span>}
              {m.api === 'responses' && <span className="badge">responses api</span>}
              <span className={`badge ${missing ? 'error' : ''}`}>
                {bound}
                {missing ? ` ${t('config.models.connectionMissing')}` : ''}
              </span>
              <span className="spacer" />
              <button className="btn danger sm" onClick={() => removeModel(name)}>
                {t('common.delete')}
              </button>
            </div>
            <label className="field">
              <span className="field-name">
                {t('config.models.description')}
                <span className="field-hint">{t('config.models.descriptionHint')}</span>
              </span>
              <textarea
                value={m.description ?? ''}
                placeholder={t('config.models.descriptionPlaceholder')}
                onChange={(e) => setModel(name, { description: e.target.value })}
              />
            </label>
            <div className="row">
              <label className="field" style={{ marginBottom: 0 }}>
                <span className="field-name">{t('config.models.provider')}</span>
                <select
                  value={m.provider ?? ''}
                  onChange={(e) => setModel(name, { provider: e.target.value || undefined })}
                >
                  <option value="">
                    {t('config.models.followDefault', { name: cfg.default_provider })}
                  </option>
                  {providerNames.map((n) => (
                    <option key={n} value={n}>{n}</option>
                  ))}
                </select>
              </label>
              <label className="field" style={{ marginBottom: 0 }}>
                <span className="field-name">
                  {t('config.models.modelNameOverride')}
                  <span className="field-hint">{t('config.models.modelNameOverrideHint')}</span>
                </span>
                <input
                  type="text"
                  className="mono"
                  value={m.model_name ?? ''}
                  placeholder={name}
                  onChange={(e) => setModel(name, { model_name: e.target.value || undefined })}
                />
              </label>
            </div>
            <div style={{ display: 'flex', gap: 18, marginTop: 12, flexWrap: 'wrap' }}>
              <label className="check">
                <input type="radio" checked={!!m.default} onChange={() => setDefault(name)} />
                {t('config.models.isDefault')}
              </label>
              <label className="check">
                <input
                  type="checkbox"
                  checked={!!m.reasoning}
                  onChange={(e) => setModel(name, { reasoning: e.target.checked })}
                />
                {t('config.models.isReasoning')}
              </label>
              <label className="check">
                <input
                  type="checkbox"
                  checked={m.api === 'responses'}
                  onChange={(e) => setModel(name, { api: e.target.checked ? 'responses' : undefined })}
                />
                Responses API
              </label>
            </div>
          </div>
        )
      })}
    </>
  )
}
