import { Fragment, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import { API_TYPES, type ApiType, type ProviderMeta } from '../../api'
import { useDialogs } from '../../components/Dialog'
import ListToolbar from './ListToolbar'
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

/** Backend connections: a set of "address + key" pairs that models reference by name.
 *
 *  One row per connection, with the facts that distinguish one from another -- type, address,
 *  whether a key is set, how many models hang off it -- in the row itself, and the editor
 *  underneath the row that was opened. Every connection used to be a permanently expanded form,
 *  which meant a handful of them filled the page and none of them could be compared. */
export default function ProvidersSection({ cfg, set, notify }: SectionProps) {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  const [revealed, setRevealed] = useState<Record<string, boolean>>({})
  const [search, setSearch] = useState('')
  const [typeFilter, setTypeFilter] = useState('')
  /** The connection whose editor is open, or null. One at a time: two open forms is the
   *  crowded page this replaces. */
  const [open, setOpen] = useState<string | null>(null)

  const providers = cfg.providers ?? {}
  const providerNames = Object.keys(providers)
  const modelNames = Object.keys(cfg.models)

  const typeOf = (name: string): ApiType => (providers[name].api_type as ApiType) ?? 'azure'

  const q = search.trim().toLowerCase()
  const shown = providerNames.filter((name) => {
    if (typeFilter && typeOf(name) !== typeFilter) return false
    if (!q) return true
    return (
      name.toLowerCase().includes(q) ||
      (providers[name].base_url ?? '').toLowerCase().includes(q)
    )
  })
  const filtered = q !== '' || typeFilter !== ''

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
    // A connection is added empty, so its editor opens on its own: an added row that shows
    // nothing but blanks would otherwise need a second click to be filled in.
    setOpen(name)
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
    if (open === oldName) setOpen(name)
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
    if (open === name) setOpen(null)
  }

  return (
    <div className="panel">
      <div className="panel-head">
        {t('config.providers.title')}
        <span className="badge">
          {t('config.providers.count', { count: providerNames.length })}
        </span>
        <span className="spacer" />
        <button className="btn ghost sm" onClick={addProvider}>
          {t('config.providers.add')}
        </button>
      </div>
      <div className="panel-body">
        <p className="panel-note" style={{ marginBottom: 0 }}>
          <Trans i18nKey="config.providers.lead" components={{ code: <code /> }} />
        </p>
      </div>

      <ListToolbar
        search={search}
        onSearch={setSearch}
        placeholder={t('config.providers.searchPlaceholder')}
        shown={shown.length}
        total={providerNames.length}
        filtered={filtered}
        onClear={() => {
          setSearch('')
          setTypeFilter('')
        }}
      >
        <label className="filter-field">
          <span className="field-name">{t('config.providers.filterType')}</span>
          <select value={typeFilter} onChange={(e) => setTypeFilter(e.target.value)}>
            <option value="">{t('config.list.all')}</option>
            {API_TYPES.map((tp) => (
              <option key={tp} value={tp}>
                {t(TYPE_LABEL[tp])}
              </option>
            ))}
          </select>
        </label>
      </ListToolbar>

      {providerNames.length === 0 ? (
        <div className="empty">{t('config.providers.empty')}</div>
      ) : shown.length === 0 ? (
        <div className="empty">{t('config.list.noMatch')}</div>
      ) : (
        <div className="table-scroll">
          <table className="cfg-table">
            <colgroup>
              <col style={{ width: '19%' }} />
              <col style={{ width: '13%' }} />
              <col style={{ width: '27%' }} />
              <col style={{ width: '9%' }} />
              <col style={{ width: '10%' }} />
              <col style={{ width: '22%' }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t('config.providers.colName')}</th>
                <th>{t('config.providers.colType')}</th>
                <th>{t('config.providers.colAddress')}</th>
                <th>{t('config.providers.colKey')}</th>
                <th>{t('config.providers.colModels')}</th>
                <th>{t('config.list.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((name) => {
                const p = providers[name]
                const isDefault = cfg.default_provider === name
                const isOpen = open === name
                const boundCount = modelNames.filter(
                  (m) => (cfg.models[m].provider ?? cfg.default_provider) === name,
                ).length
                const apiType = typeOf(name)
                const version = VERSION_FIELD[apiType]
                return (
                  <Fragment key={name}>
                    <tr
                      className={isOpen ? 'selected' : ''}
                      onClick={() => setOpen(isOpen ? null : name)}
                      title={t(isOpen ? 'config.list.collapse' : 'config.list.expand')}
                    >
                      <td className="truncate">
                        <span className="expander">{isOpen ? '▾' : '▸'}</span>
                        <span className="cell-name">{name}</span>
                        {isDefault && (
                          <span className="badge ok" style={{ marginLeft: 6 }}>
                            {t('config.providers.defaultBadge')}
                          </span>
                        )}
                      </td>
                      <td className="truncate">
                        <span className="badge">{t(TYPE_LABEL[apiType])}</span>
                      </td>
                      <td className="truncate" title={p.base_url ?? ''}>
                        {p.base_url ? (
                          <span className="mono">{p.base_url}</span>
                        ) : (
                          <span className="dim">{t('config.providers.noAddress')}</span>
                        )}
                      </td>
                      <td className="truncate">
                        <span className={`badge ${p.api_key ? 'ok' : 'warn'}`}>
                          {t(p.api_key ? 'config.providers.keySet' : 'config.providers.keyUnset')}
                        </span>
                      </td>
                      <td className="num">{boundCount}</td>
                      <td>
                        <span className="cell-acts" onClick={(e) => e.stopPropagation()}>
                          {!isDefault && (
                            <button
                              className="btn subtle sm"
                              onClick={() => set({ default_provider: name })}
                            >
                              {t('config.providers.setDefault')}
                            </button>
                          )}
                          <button className="btn ghost sm" onClick={() => renameProvider(name)}>
                            {t('config.providers.rename')}
                          </button>
                          <button className="btn danger sm" onClick={() => removeProvider(name)}>
                            {t('common.delete')}
                          </button>
                        </span>
                      </td>
                    </tr>

                    {isOpen && (
                      <tr className="editor-tr">
                        <td className="editor-cell" colSpan={6}>
                          <label className="field">
                            <span className="field-name">
                              {t('config.providers.baseUrl')}
                              <span className="field-hint">
                                {t('config.providers.baseUrlHint.' + apiType)}
                              </span>
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
                                  // The stored version belongs to the type it was entered
                                  // under, so it is cleared on a switch: an Azure api-version
                                  // string sent as anthropic-version is rejected upstream, and
                                  // a stale value in a field the user never revisits is the
                                  // kind of thing nobody thinks to check.
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
                                  onChange={(e) =>
                                    setProvider(name, { api_version: e.target.value })
                                  }
                                />
                              </label>
                            )}
                          </div>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      <div className="list-footer">
        <span className="dim">{t('config.list.rowHint')}</span>
      </div>
    </div>
  )
}
