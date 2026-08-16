import { useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import type { ProviderMeta } from '../../api'
import type { SectionProps } from './types'

const BLANK_PROVIDER: ProviderMeta = {
  base_url: '',
  api_key: '',
  api_type: 'azure',
  api_version: '2024-12-01-preview',
}

/** Backend connections: a set of "address + key" pairs that models reference by name. */
export default function ProvidersSection({ cfg, set, notify }: SectionProps) {
  const { t } = useTranslation()
  const [revealed, setRevealed] = useState<Record<string, boolean>>({})

  const providers = cfg.providers ?? {}
  const providerNames = Object.keys(providers)
  const modelNames = Object.keys(cfg.models)

  const setProvider = (name: string, patch: Partial<ProviderMeta>) =>
    set({ providers: { ...providers, [name]: { ...providers[name], ...patch } } })

  const addProvider = () => {
    const name = prompt(t('config.providers.promptName'))?.trim()
    if (!name) return
    if (providers[name]) {
      notify('error', t('config.providers.alreadyExists', { name }))
      return
    }
    set({ providers: { ...providers, [name]: { ...BLANK_PROVIDER } } })
  }

  const renameProvider = (oldName: string) => {
    const name = prompt(t('config.providers.promptRename'), oldName)?.trim()
    if (!name || name === oldName) return
    if (providers[name]) {
      notify('error', t('config.providers.alreadyExists', { name }))
      return
    }
    const next = Object.fromEntries(
      Object.entries(providers).map(([k, v]) => [k === oldName ? name : k, v]),
    )
    const models = Object.fromEntries(
      Object.entries(cfg.models).map(([k, v]) => [
        k,
        v.provider === oldName ? { ...v, provider: name } : v,
      ]),
    )
    set({
      providers: next,
      models,
      default_provider: cfg.default_provider === oldName ? name : cfg.default_provider,
      ai_router:
        cfg.ai_router.decision_provider === oldName
          ? { ...cfg.ai_router, decision_provider: name }
          : cfg.ai_router,
    })
  }

  const removeProvider = (name: string) => {
    const used = modelNames.filter((m) => cfg.models[m].provider === name)
    if (used.length) {
      notify(
        'error',
        t('config.providers.stillReferenced', {
          name,
          models: used.join(t('common.listSeparator')),
        }),
      )
      return
    }
    if (cfg.default_provider === name) {
      notify('error', t('config.providers.isDefaultConnection', { name }))
      return
    }
    const { [name]: _omit, ...rest } = providers
    set({ providers: rest })
  }

  return (
    <>
      <div className="panel">
        <div className="panel-head">
          {t('config.providers.title')}
          <span className="badge">{t('config.providers.count', { count: providerNames.length })}</span>
          <span className="spacer" />
          <button className="btn ghost sm" onClick={addProvider}>{t('config.providers.add')}</button>
        </div>
        <div className="panel-body">
          <p className="panel-note" style={{ marginBottom: 0 }}>
            <Trans i18nKey="config.providers.lead" components={{ code: <code /> }} />
          </p>
        </div>
      </div>

      {providerNames.length === 0 && (
        <div className="panel">
          <div className="empty">{t('config.providers.empty')}</div>
        </div>
      )}

      {providerNames.map((name) => {
        const p = providers[name]
        const isDefault = cfg.default_provider === name
        const boundCount = modelNames.filter(
          (m) => (cfg.models[m].provider ?? cfg.default_provider) === name,
        ).length
        return (
          <div className="provider-card" key={name}>
            <div className="head">
              <span className="name">{name}</span>
              {isDefault && <span className="badge ok">{t('config.providers.defaultBadge')}</span>}
              <span className="badge">
                {p.api_type === 'openai'
                  ? t('config.providers.typeOpenAI')
                  : t('config.providers.typeAzure')}
              </span>
              <span className="badge">{t('config.providers.boundModels', { count: boundCount })}</span>
              <span className="spacer" />
              {!isDefault && (
                <button className="btn subtle sm" onClick={() => set({ default_provider: name })}>
                  {t('config.providers.setDefault')}
                </button>
              )}
              <button className="btn ghost sm" onClick={() => renameProvider(name)}>
                {t('config.providers.rename')}
              </button>
              <button className="btn danger sm" onClick={() => removeProvider(name)}>
                {t('common.delete')}
              </button>
            </div>

            <label className="field">
              <span className="field-name">
                {t('config.providers.baseUrl')}
                <span className="field-hint">{t('config.providers.baseUrlHint')}</span>
              </span>
              <input
                type="text"
                className="mono"
                value={p.base_url ?? ''}
                placeholder="https://your-resource.openai.azure.com/"
                onChange={(e) => setProvider(name, { base_url: e.target.value })}
              />
            </label>

            <label className="field">
              <span className="field-name">
                {t('config.providers.apiKey')}
                <span className="field-hint">{t('config.providers.apiKeyHint')}</span>
              </span>
              <span style={{ display: 'flex', gap: 8 }}>
                <input
                  type={revealed[name] ? 'text' : 'password'}
                  className="mono"
                  value={p.api_key ?? ''}
                  placeholder={t('config.providers.apiKeyPlaceholder')}
                  onChange={(e) => setProvider(name, { api_key: e.target.value })}
                />
                <button
                  className="btn ghost sm"
                  style={{ flex: 'none' }}
                  onClick={() => setRevealed((r) => ({ ...r, [name]: !r[name] }))}
                >
                  {revealed[name] ? t('common.hide') : t('common.show')}
                </button>
              </span>
            </label>

            <div className="row">
              <label className="field" style={{ marginBottom: 0 }}>
                <span className="field-name">{t('config.providers.apiType')}</span>
                <select
                  value={p.api_type ?? 'azure'}
                  onChange={(e) =>
                    setProvider(name, { api_type: e.target.value as 'azure' | 'openai' })
                  }
                >
                  <option value="azure">{t('config.providers.apiTypeAzureOption')}</option>
                  <option value="openai">{t('config.providers.apiTypeOpenAIOption')}</option>
                </select>
              </label>
              <label className="field" style={{ marginBottom: 0 }}>
                <span className="field-name">
                  api_version
                  <span className="field-hint">{t('config.providers.apiVersionHint')}</span>
                </span>
                <input
                  type="text"
                  className="mono"
                  value={p.api_version ?? ''}
                  placeholder="2024-12-01-preview"
                  disabled={p.api_type === 'openai'}
                  onChange={(e) => setProvider(name, { api_version: e.target.value })}
                />
              </label>
            </div>
          </div>
        )
      })}
    </>
  )
}
