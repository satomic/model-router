import { Trans, useTranslation } from 'react-i18next'
import type { Rule } from '../../api'
import type { SectionProps } from './types'

/** Rule routing: evaluated in order, the first match decides the model. */
export default function RulesSection({ cfg, set, goto }: SectionProps) {
  const { t } = useTranslation()
  const modelNames = Object.keys(cfg.models)
  const defaultModel = modelNames.find((n) => cfg.models[n].default)

  const setRule = (i: number, patch: Partial<Rule>) =>
    set({ rules: cfg.rules.map((r, j) => (j === i ? { ...r, ...patch } : r)) })

  const moveRule = (i: number, dir: -1 | 1) => {
    const rules = [...cfg.rules]
    const j = i + dir
    if (j < 0 || j >= rules.length) return
    ;[rules[i], rules[j]] = [rules[j], rules[i]]
    set({ rules })
  }

  const addRule = () =>
    set({
      rules: [
        ...cfg.rules,
        { name: `rule-${cfg.rules.length + 1}`, keywords: [], model: modelNames[0] },
      ],
    })

  return (
    <>
      <div className="panel">
        <div className="panel-head">
          {t('config.rules.title')}
          <span className="badge">{t('config.rules.count', { count: cfg.rules.length })}</span>
          {cfg.strategy !== 'rule' && <span className="badge">{t('config.rules.inactive')}</span>}
          <span className="spacer" />
          <button className="btn ghost sm" onClick={addRule}>{t('config.rules.add')}</button>
        </div>
        <div className="panel-body">
          {/* One key for the whole paragraph: the default-model badge, the "not set" case and
              the link to the strategy page all sit inside the sentence, and CJK/EN word order
              differs, so it must never be assembled from fragments. */}
          <p className="panel-note" style={{ marginBottom: 0 }}>
            <Trans
              i18nKey="config.rules.lead"
              values={{ model: defaultModel ?? t('config.rules.noDefaultModel') }}
              components={{
                strong: <strong />,
                model: defaultModel ? (
                  <span className="badge model" />
                ) : (
                  <span className="dim" />
                ),
                strategyLink: (
                  <button className="btn subtle sm" onClick={() => goto('strategy')} />
                ),
              }}
            />
          </p>
        </div>
      </div>

      {cfg.rules.length === 0 && (
        <div className="panel">
          <div className="empty">{t('config.rules.empty')}</div>
        </div>
      )}

      {cfg.rules.map((r, i) => (
        <div className="rule-card" key={i}>
          <div className="head">
            <span className="mono dim">#{i + 1}</span>
            <input
              type="text"
              value={r.name}
              style={{ width: 180, flex: 'none' }}
              onChange={(e) => setRule(i, { name: e.target.value })}
            />
            <span className="dim mono">→</span>
            <select
              value={r.model}
              onChange={(e) => setRule(i, { model: e.target.value })}
              style={{ width: 170, flex: 'none' }}
            >
              {modelNames.map((n) => (
                <option key={n} value={n}>{n}</option>
              ))}
            </select>
            <span className="spacer" />
            <button
              className="btn ghost sm"
              onClick={() => moveRule(i, -1)}
              disabled={i === 0}
              title={t('config.rules.moveUp')}
            >
              ↑
            </button>
            <button
              className="btn ghost sm"
              onClick={() => moveRule(i, 1)}
              disabled={i === cfg.rules.length - 1}
              title={t('config.rules.moveDown')}
            >
              ↓
            </button>
            <button
              className="btn danger sm"
              onClick={() => set({ rules: cfg.rules.filter((_, j) => j !== i) })}
            >
              {t('common.delete')}
            </button>
          </div>
          <div className="row">
            <label className="field" style={{ flex: 3, marginBottom: 0 }}>
              <span className="field-name">
                {t('config.rules.keywords')}
                <span className="field-hint">{t('config.rules.keywordsHint')}</span>
              </span>
              <input
                type="text"
                value={(r.keywords ?? []).join(', ')}
                placeholder={t('config.rules.keywordsPlaceholder')}
                onChange={(e) =>
                  setRule(i, {
                    // Accepts the full-width comma too, since a CJK keyboard produces it by default
                    keywords: e.target.value.split(/[,，]/).map((s) => s.trim()).filter(Boolean),
                  })
                }
              />
            </label>
            <label className="field" style={{ marginBottom: 0 }}>
              <span className="field-name">
                {t('config.rules.minPromptChars')}
                <span className="field-hint">{t('config.rules.minPromptCharsHint')}</span>
              </span>
              <input
                type="number"
                value={r.min_prompt_chars ?? ''}
                placeholder={t('config.rules.disabled')}
                onChange={(e) =>
                  setRule(i, { min_prompt_chars: e.target.value ? +e.target.value : undefined })
                }
              />
            </label>
          </div>
        </div>
      ))}
    </>
  )
}
