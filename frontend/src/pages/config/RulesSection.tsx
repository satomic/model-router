import { Fragment, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import type { Rule } from '../../api'
import ListToolbar from './ListToolbar'
import { rulesActive } from './strategy'
import type { SectionProps } from './types'

/** Rule routing: evaluated in order, the first match decides the model.
 *
 *  Order is the whole point of this page, so it is a numbered table: the sequence a request is
 *  matched against reads straight down the first column, with each rule's conditions and target
 *  on one line, and the editor opens under the rule being changed. */
export default function RulesSection({ cfg, set, goto }: SectionProps) {
  const { t } = useTranslation()
  const modelNames = Object.keys(cfg.models)
  const defaultModel = modelNames.find((n) => cfg.models[n].default)

  const [search, setSearch] = useState('')
  const [modelFilter, setModelFilter] = useState('')
  /** Index of the open rule, or null. Rules have no stable id, so position is the identity --
   *  which is also why reordering closes the editor. */
  const [open, setOpen] = useState<number | null>(null)

  const q = search.trim().toLowerCase()
  // Carrying the real index alongside each rule, because every mutation below addresses a rule
  // by its position in cfg.rules and a filtered list would otherwise edit the wrong one.
  const shown = cfg.rules
    .map((r, i) => ({ r, i }))
    .filter(({ r }) => {
      if (modelFilter && r.model !== modelFilter) return false
      if (!q) return true
      return (
        r.name.toLowerCase().includes(q) ||
        (r.keywords ?? []).some((k) => k.toLowerCase().includes(q))
      )
    })
  const filtered = q !== '' || modelFilter !== ''

  const setRule = (i: number, patch: Partial<Rule>) =>
    set({ rules: cfg.rules.map((r, j) => (j === i ? { ...r, ...patch } : r)) })

  const moveRule = (i: number, dir: -1 | 1) => {
    const rules = [...cfg.rules]
    const j = i + dir
    if (j < 0 || j >= rules.length) return
    ;[rules[i], rules[j]] = [rules[j], rules[i]]
    set({ rules })
    // The open editor is addressed by index, so it follows the rule that moved.
    if (open === i) setOpen(j)
    else if (open === j) setOpen(i)
  }

  const addRule = () => {
    set({
      rules: [
        ...cfg.rules,
        { name: `rule-${cfg.rules.length + 1}`, keywords: [], model: modelNames[0] },
      ],
    })
    // A rule with no keywords and no length matches everything, so the new row opens for its
    // conditions to be filled in rather than sitting in the list as a catch-all.
    setOpen(cfg.rules.length)
  }

  const removeRule = (i: number) => {
    set({ rules: cfg.rules.filter((_, j) => j !== i) })
    if (open === i) setOpen(null)
    else if (open !== null && open > i) setOpen(open - 1)
  }

  return (
    <div className="panel">
      <div className="panel-head">
        {t('config.rules.title')}
        <span className="badge">{t('config.rules.count', { count: cfg.rules.length })}</span>
        {!rulesActive(cfg.strategy) && <span className="badge">{t('config.rules.inactive')}</span>}
        <span className="spacer" />
        <button className="btn ghost sm" onClick={addRule}>
          {t('config.rules.add')}
        </button>
      </div>
      <div className="panel-body">
        {/* One key for the whole paragraph: the default-model badge, the "not set" case and
            the link to the strategy page all sit inside the sentence, and CJK/EN word order
            differs, so it must never be assembled from fragments.

            Two variants, because what happens to an unmatched request is the difference
            between the two strategies that consult these rules: under `rule` it goes to the
            default model, under `rule-then-ai` to the decision model. Stating the wrong one
            would make the rules page describe behaviour the router does not have. */}
        <p className="panel-note" style={{ marginBottom: 0 }}>
          <Trans
            i18nKey={
              cfg.strategy === 'rule-then-ai' ? 'config.rules.leadThenAi' : 'config.rules.lead'
            }
            values={{ model: defaultModel ?? t('config.rules.noDefaultModel') }}
            components={{
              strong: <strong />,
              model: defaultModel ? <span className="badge model" /> : <span className="dim" />,
              strategyLink: <button className="btn subtle sm" onClick={() => goto('strategy')} />,
            }}
          />
        </p>
      </div>

      <ListToolbar
        search={search}
        onSearch={setSearch}
        placeholder={t('config.rules.searchPlaceholder')}
        shown={shown.length}
        total={cfg.rules.length}
        filtered={filtered}
        onClear={() => {
          setSearch('')
          setModelFilter('')
        }}
      >
        <label className="filter-field">
          <span className="field-name">{t('config.rules.filterModel')}</span>
          <select value={modelFilter} onChange={(e) => setModelFilter(e.target.value)}>
            <option value="">{t('config.list.all')}</option>
            {modelNames.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
      </ListToolbar>

      {cfg.rules.length === 0 ? (
        <div className="empty">{t('config.rules.empty')}</div>
      ) : shown.length === 0 ? (
        <div className="empty">{t('config.list.noMatch')}</div>
      ) : (
        <div className="table-scroll">
          <table className="cfg-table">
            <colgroup>
              <col style={{ width: '6%' }} />
              <col style={{ width: '20%' }} />
              <col style={{ width: '34%' }} />
              <col style={{ width: '19%' }} />
              <col style={{ width: '21%' }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t('config.rules.colOrder')}</th>
                <th>{t('config.rules.colName')}</th>
                <th>{t('config.rules.colConditions')}</th>
                <th>{t('config.rules.colModel')}</th>
                <th>{t('config.list.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {shown.map(({ r, i }) => {
                const isOpen = open === i
                const keywords = r.keywords ?? []
                const hasConditions = keywords.length > 0 || !!r.min_prompt_chars
                return (
                  <Fragment key={i}>
                    <tr
                      className={isOpen ? 'selected' : ''}
                      onClick={() => setOpen(isOpen ? null : i)}
                      title={t(isOpen ? 'config.list.collapse' : 'config.list.expand')}
                    >
                      <td className="mono dim num">{i + 1}</td>
                      <td className="truncate">
                        <span className="expander">{isOpen ? '▾' : '▸'}</span>
                        <span className="cell-name">{r.name}</span>
                      </td>
                      <td className="truncate">
                        {hasConditions ? (
                          <span className="cell-badges">
                            {keywords.map((k) => (
                              <span className="badge" key={k}>
                                {k}
                              </span>
                            ))}
                            {!!r.min_prompt_chars && (
                              <span className="badge warn">
                                {t('config.rules.minChars', { n: r.min_prompt_chars })}
                              </span>
                            )}
                          </span>
                        ) : (
                          <span className="dim">{t('config.rules.noConditions')}</span>
                        )}
                      </td>
                      <td className="truncate">
                        <span className="badge model">{r.model}</span>
                      </td>
                      <td>
                        <span className="cell-acts" onClick={(e) => e.stopPropagation()}>
                          {/* Reordering is by position, so it is refused while a filter hides
                              part of the list: "move up" past a hidden neighbour would move
                              the rule somewhere the user cannot see. */}
                          <button
                            className="btn ghost sm"
                            onClick={() => moveRule(i, -1)}
                            disabled={filtered || i === 0}
                            title={
                              filtered ? t('config.rules.reorderLocked') : t('config.rules.moveUp')
                            }
                          >
                            ↑
                          </button>
                          <button
                            className="btn ghost sm"
                            onClick={() => moveRule(i, 1)}
                            disabled={filtered || i === cfg.rules.length - 1}
                            title={
                              filtered
                                ? t('config.rules.reorderLocked')
                                : t('config.rules.moveDown')
                            }
                          >
                            ↓
                          </button>
                          <button className="btn danger sm" onClick={() => removeRule(i)}>
                            {t('common.delete')}
                          </button>
                        </span>
                      </td>
                    </tr>

                    {isOpen && (
                      <tr className="editor-tr">
                        <td className="editor-cell" colSpan={5}>
                          <div className="row">
                            <label className="field" style={{ flex: 2, marginBottom: 0 }}>
                              <span className="field-name">{t('config.rules.colName')}</span>
                              <input
                                type="text"
                                value={r.name}
                                onChange={(e) => setRule(i, { name: e.target.value })}
                              />
                            </label>
                            <label className="field" style={{ flex: 2, marginBottom: 0 }}>
                              <span className="field-name">{t('config.rules.colModel')}</span>
                              <select
                                value={r.model}
                                onChange={(e) => setRule(i, { model: e.target.value })}
                              >
                                {modelNames.map((n) => (
                                  <option key={n} value={n}>
                                    {n}
                                  </option>
                                ))}
                              </select>
                            </label>
                          </div>
                          <div className="row" style={{ marginTop: 12 }}>
                            <label className="field" style={{ flex: 3, marginBottom: 0 }}>
                              <span className="field-name">
                                {t('config.rules.keywords')}
                                <span className="field-hint">{t('config.rules.keywordsHint')}</span>
                              </span>
                              <input
                                type="text"
                                value={keywords.join(', ')}
                                placeholder={t('config.rules.keywordsPlaceholder')}
                                onChange={(e) =>
                                  setRule(i, {
                                    // Accepts the full-width comma too, since a CJK keyboard
                                    // produces it by default
                                    keywords: e.target.value
                                      .split(/[,，]/)
                                      .map((s) => s.trim())
                                      .filter(Boolean),
                                  })
                                }
                              />
                            </label>
                            <label className="field" style={{ marginBottom: 0 }}>
                              <span className="field-name">
                                {t('config.rules.minPromptChars')}
                                <span className="field-hint">
                                  {t('config.rules.minPromptCharsHint')}
                                </span>
                              </span>
                              <input
                                type="number"
                                value={r.min_prompt_chars ?? ''}
                                placeholder={t('config.rules.disabled')}
                                onChange={(e) =>
                                  setRule(i, {
                                    min_prompt_chars: e.target.value
                                      ? +e.target.value
                                      : undefined,
                                  })
                                }
                              />
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
