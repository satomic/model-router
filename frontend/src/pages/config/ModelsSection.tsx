import { Fragment, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import type { ModelMeta } from '../../api'
import { useDialogs } from '../../components/Dialog'
import ListToolbar from './ListToolbar'
import type { SectionProps } from './types'

/** The three capabilities a model row can carry, and the filter values they answer to. */
const TRAITS = [
  { key: 'default', label: 'config.models.defaultBadge' },
  { key: 'reasoning', label: 'config.models.reasoningBadge' },
  { key: 'responses', label: 'config.models.traitResponses' },
] as const

/** Model catalog: the names exposed to clients, the connection each is bound to, and the
 *  descriptions the AI decision model reads.
 *
 *  One row per model, because the questions asked of this page are comparisons -- which model is
 *  the default, which connection does each go to, which ones are missing a description the
 *  decision model needs -- and a column answers those at a glance where a stack of expanded
 *  forms answered none of them. The form itself opens under the row it belongs to. */
export default function ModelsSection({ cfg, set, notify, goto }: SectionProps) {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  const providerNames = Object.keys(cfg.providers ?? {})
  const modelNames = Object.keys(cfg.models)

  const [search, setSearch] = useState('')
  const [connFilter, setConnFilter] = useState('')
  const [traitFilter, setTraitFilter] = useState('')
  const [open, setOpen] = useState<string | null>(null)

  /** The connection a model actually resolves to, which is what the column has to show: a model
   *  with no `provider` of its own follows `default_provider`. */
  const boundOf = (name: string) => cfg.models[name].provider ?? cfg.default_provider

  const q = search.trim().toLowerCase()
  const shown = modelNames.filter((name) => {
    const m = cfg.models[name]
    if (connFilter && boundOf(name) !== connFilter) return false
    if (traitFilter === 'default' && !m.default) return false
    if (traitFilter === 'reasoning' && !m.reasoning) return false
    if (traitFilter === 'responses' && m.api !== 'responses') return false
    if (!q) return true
    return (
      name.toLowerCase().includes(q) ||
      (m.description ?? '').toLowerCase().includes(q) ||
      (m.model_name ?? '').toLowerCase().includes(q)
    )
  })
  const filtered = q !== '' || connFilter !== '' || traitFilter !== ''

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

  const addModel = async () => {
    const name = await dialogs.prompt({
      title: t('config.models.add'),
      message: t('config.models.promptName'),
      label: t('config.models.promptLabel'),
      placeholder: 'gpt-4.1-mini',
      mono: true,
      validate: (v) => (cfg.models[v] ? t('config.models.alreadyExists', { name: v }) : null),
    })
    if (!name) return
    set({ models: { ...cfg.models, [name]: { description: '' } } })
    // A new model has no description, and the description is the one field the AI decision
    // model actually reads, so its editor opens straight away.
    setOpen(name)
  }

  const removeModel = (name: string) => {
    const affected = cfg.rules.filter((r) => r.model === name)
    const { [name]: _omit, ...rest } = cfg.models
    // Model groups are pruned in the same patch. The backend rejects a group naming a model that
    // is not in the catalog, so leaving the name behind would make the config unsavable -- and a
    // group that appears to grant a model it cannot is worse than one that lost an entry.
    const groups = cfg.model_groups
    const affectedGroups = Object.entries(groups ?? {}).filter(([, ms]) => ms.includes(name))
    set({
      models: rest,
      rules: cfg.rules.filter((r) => r.model !== name),
      ...(affectedGroups.length
        ? {
            model_groups: Object.fromEntries(
              Object.entries(groups ?? {}).map(([g, ms]) => [g, ms.filter((m) => m !== name)]),
            ),
          }
        : {}),
    })
    if (open === name) setOpen(null)
    if (affected.length || affectedGroups.length) {
      notify(
        'ok',
        t('config.models.removedWithRefs', {
          name,
          rules: affected.length,
          groups: affectedGroups.length,
        }),
      )
    }
  }

  return (
    <div className="panel">
      <div className="panel-head">
        {t('config.models.title')}
        <span className="badge">{t('config.models.count', { count: modelNames.length })}</span>
        <span className="spacer" />
        <button className="btn ghost sm" onClick={addModel}>
          {t('config.models.add')}
        </button>
      </div>
      <div className="panel-body">
        <p className="panel-note" style={{ marginBottom: 0 }}>
          <Trans
            i18nKey="config.models.lead"
            components={{
              providersLink: <button className="btn subtle sm" onClick={() => goto('providers')} />,
            }}
          />
        </p>
      </div>

      <ListToolbar
        search={search}
        onSearch={setSearch}
        placeholder={t('config.models.searchPlaceholder')}
        shown={shown.length}
        total={modelNames.length}
        filtered={filtered}
        onClear={() => {
          setSearch('')
          setConnFilter('')
          setTraitFilter('')
        }}
      >
        <label className="filter-field">
          <span className="field-name">{t('config.models.filterConnection')}</span>
          <select value={connFilter} onChange={(e) => setConnFilter(e.target.value)}>
            <option value="">{t('config.list.all')}</option>
            {providerNames.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <label className="filter-field">
          <span className="field-name">{t('config.models.filterTrait')}</span>
          <select value={traitFilter} onChange={(e) => setTraitFilter(e.target.value)}>
            <option value="">{t('config.list.all')}</option>
            {TRAITS.map((tr) => (
              <option key={tr.key} value={tr.key}>
                {t(tr.label)}
              </option>
            ))}
          </select>
        </label>
      </ListToolbar>

      {modelNames.length === 0 ? (
        <div className="empty">{t('config.models.empty')}</div>
      ) : shown.length === 0 ? (
        <div className="empty">{t('config.list.noMatch')}</div>
      ) : (
        <div className="table-scroll">
          <table className="cfg-table">
            <colgroup>
              <col style={{ width: '19%' }} />
              <col style={{ width: '14%' }} />
              <col style={{ width: '15%' }} />
              <col style={{ width: '18%' }} />
              <col style={{ width: '24%' }} />
              <col style={{ width: '10%' }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t('config.models.colName')}</th>
                <th>{t('config.models.colConnection')}</th>
                <th>{t('config.models.colUpstream')}</th>
                <th>{t('config.models.colTraits')}</th>
                <th>{t('config.models.colDescription')}</th>
                <th>{t('config.list.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((name) => {
                const m = cfg.models[name]
                const bound = boundOf(name)
                const missing = !cfg.providers?.[bound]
                const isOpen = open === name
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
                      </td>
                      <td className="truncate">
                        <span className={`badge ${missing ? 'error' : ''}`}>
                          {bound}
                          {missing ? ` ${t('config.models.connectionMissing')}` : ''}
                        </span>
                      </td>
                      {/* mono on the value, not on the cell: "same as the model name" is prose
                          and read as an identifier when it wore the monospace face. */}
                      <td className="truncate" title={m.model_name ?? ''}>
                        {m.model_name ? (
                          <span className="mono">{m.model_name}</span>
                        ) : (
                          <span className="dim">{t('config.models.sameAsName')}</span>
                        )}
                      </td>
                      <td className="truncate">
                        <span className="cell-badges">
                          {m.default && (
                            <span className="badge ok">{t('config.models.defaultBadge')}</span>
                          )}
                          {m.reasoning && (
                            <span className="badge warn">{t('config.models.reasoningBadge')}</span>
                          )}
                          {m.api === 'responses' && <span className="badge">responses api</span>}
                        </span>
                      </td>
                      <td className="truncate" title={m.description ?? ''}>
                        {m.description ? m.description : (
                          <span className="dim">{t('config.models.noDescription')}</span>
                        )}
                      </td>
                      <td>
                        <span className="cell-acts" onClick={(e) => e.stopPropagation()}>
                          <button className="btn danger sm" onClick={() => removeModel(name)}>
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
                              {t('config.models.description')}
                              <span className="field-hint">
                                {t('config.models.descriptionHint')}
                              </span>
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
                                onChange={(e) =>
                                  setModel(name, { provider: e.target.value || undefined })
                                }
                              >
                                <option value="">
                                  {t('config.models.followDefault', {
                                    name: cfg.default_provider,
                                  })}
                                </option>
                                {providerNames.map((n) => (
                                  <option key={n} value={n}>
                                    {n}
                                  </option>
                                ))}
                              </select>
                            </label>
                            <label className="field" style={{ marginBottom: 0 }}>
                              <span className="field-name">
                                {t('config.models.modelNameOverride')}
                                <span className="field-hint">
                                  {t('config.models.modelNameOverrideHint')}
                                </span>
                              </span>
                              <input
                                type="text"
                                className="mono"
                                value={m.model_name ?? ''}
                                placeholder={name}
                                onChange={(e) =>
                                  setModel(name, { model_name: e.target.value || undefined })
                                }
                              />
                            </label>
                          </div>
                          <div
                            style={{ display: 'flex', gap: 18, marginTop: 12, flexWrap: 'wrap' }}
                          >
                            <label className="check">
                              <input
                                type="radio"
                                checked={!!m.default}
                                onChange={() => setDefault(name)}
                              />
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
                                onChange={(e) =>
                                  setModel(name, {
                                    api: e.target.checked ? 'responses' : undefined,
                                  })
                                }
                              />
                              Responses API
                            </label>
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
