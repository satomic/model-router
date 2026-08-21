import { useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import { API_TYPES, type ApiType, type ProviderMeta } from '../../api'
import { useDialogs } from '../../components/Dialog'
import type { SectionProps } from './types'

const BLANK_PROVIDER: ProviderMeta = {
  base_url: '',
  api_key: '',
  api_type: 'azure',
  api_version: '2024-12-01-preview',
}

/** The label for one connection type, so the badge and the picker cannot drift apart. */
const TYPE_LABEL: Record<ApiType, string> = {
  azure: 'config.providers.typeAzure',
  openai: 'config.providers.typeOpenAI',
  anthropic: 'config.providers.typeAnthropic',
}

/** The version field means a different thing per connection type, so it is labelled and
 *  defaulted per type rather than shown as one generic "api_version" for all three. */
const VERSION_FIELD: Record<ApiType, { label: string; hint: string; placeholder: string } | null> = {
  azure: {
    label: 'api_version',
    hint: 'config.providers.apiVersionHint',
    placeholder: '2024-12-01-preview',
  },
  // An OpenAI-compatible address carries no version parameter at all.
  openai: null,
  anthropic: {
    label: 'anthropic-version',
    hint: 'config.providers.anthropicVersionHint',
    placeholder: '2023-06-01',
  },
}

/** Backend connections: a set of "address + key" pairs that models reference by name. */
export default function ProvidersSection({ cfg, set, notify }: SectionProps) {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  const [revealed, setRevealed] = useState<Record<string, boolean>>({})

  const providers = cfg.providers ?? {}
  const providerNames = Object.keys(providers)
  const modelNames = Object.keys(cfg.models)

  const setProvider = (name: string, patch: Partial<ProviderMeta>) =>
    set({ providers: { ...providers, [name]: { ...providers[name], ...patch } } })

  const addProvider = async () => {
    const name = await dialogs.prompt({
      title: t('config.providers.add'),
      message: t('config.providers.promptName'),
      label: t('config.providers.promptLabel'),
      placeholder: 'openrouter',
      mono: true,
      // Validated in the dialog rather than reported afterwards, so the name is corrected in
      // the field it was typed into instead of the dialog closing on a rejected value.
      validate: (v) => (providers[v] ? t('config.providers.alreadyExists', { name: v }) : null),
    })
    if (!name) return
    set({ providers: { ...providers, [name]: { ...BLANK_PROVIDER } } })
  }

  const renameProvider = async (oldName: string) => {
    const name = await dialogs.prompt({
      title: t('config.providers.rename'),
      message: t('config.providers.promptRename'),
      label: t('config.providers.promptLabel'),
      defaultValue: oldName,
      mono: true,
      validate: (v) =>
        v !== oldName && providers[v] ? t('config.providers.alreadyExists', { name: v }) : null,
    })
    if (!name || name === oldName) return
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
        const apiType: ApiType = (p.api_type as ApiType) ?? 'azure'
        const version = VERSION_FIELD[apiType]
        return (
          <div className="provider-card" key={name}>
            <div className="head">
              <span className="name">{name}</span>
              {isDefault && <span className="badge ok">{t('config.providers.defaultBadge')}</span>}
              <span className="badge">{t(TYPE_LABEL[apiType])}</span>
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
                <span className="field-hint">{t('config.providers.baseUrlHint.' + apiType)}</span>
              </span>
              <input
                type="text"
                className="mono"
                value={p.base_url ?? ''}
                placeholder={t('config.providers.baseUrlPlaceholder.' + apiType)}
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
                  value={apiType}
                  onChange={(e) => {
                    const next = e.target.value as ApiType
                    // The stored version belongs to the type it was entered under, so it is
                    // cleared on a switch: an Azure api-version string sent as
                    // anthropic-version is rejected upstream, and a stale value in a field
                    // the user never revisits is the kind of thing nobody thinks to check.
                    setProvider(name, { api_type: next, api_version: '' })
                  }}
                >
                  {API_TYPES.map((tp) => (
                    <option key={tp} value={tp}>
                      {t('config.providers.apiTypeOption.' + tp)}
                    </option>
                  ))}
                </select>
              </label>
              {version && (
                <label className="field" style={{ marginBottom: 0 }}>
                  <span className="field-name">
                    {version.label}
                    <span className="field-hint">{t(version.hint)}</span>
                  </span>
                  <input
                    type="text"
                    className="mono"
                    value={p.api_version ?? ''}
                    placeholder={version.placeholder}
                    onChange={(e) => setProvider(name, { api_version: e.target.value })}
                  />
                </label>
              )}
            </div>
          </div>
        )
      })}
    </>
  )
}
