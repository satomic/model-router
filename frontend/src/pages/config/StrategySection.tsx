import { Trans, useTranslation } from 'react-i18next'
import { aiRouterActive } from './strategy'
import DecisionPromptPanel from './DecisionPromptPanel'
import RoutingFlow from './RoutingFlow'
import type { SectionProps } from './types'

/** Routing strategy: rule / ai / rule-then-ai, session stickiness, and the AI decision model's
 *  parameters and prompt. */
export default function StrategySection({ cfg, set, notify, goto }: SectionProps) {
  const { t } = useTranslation()
  const providerNames = Object.keys(cfg.providers ?? {})

  /** The rules link inside a choice card's description. The card is a `<label>`, so a click on
   *  the button would also select its radio -- hence the stopPropagation. */
  const rulesLink = (
    <button
      className="btn subtle sm"
      onClick={(e) => {
        e.preventDefault()
        e.stopPropagation()
        goto('rules')
      }}
    />
  )

  return (
    <>
      {/* The picture comes before the picker: what changes when the radio moves is the chain
          below, and seeing it is what makes the three options comparable. */}
      <RoutingFlow cfg={cfg} goto={goto} />

      <div className="panel">
        <div className="panel-head">{t('config.strategy.title')}</div>
        <div className="panel-body">
          <div className="choice-list">
            {/* rule-then-ai is listed first: it is the strategy that needs no trade-off
                explained, since the rules cost nothing and the decision call is only paid for
                a request they did not answer. */}
            <label className={`choice ${cfg.strategy === 'rule-then-ai' ? 'selected' : ''}`}>
              <input
                type="radio"
                checked={cfg.strategy === 'rule-then-ai'}
                onChange={() => set({ strategy: 'rule-then-ai' })}
              />
              <span>
                <span className="choice-title">
                  {t('config.strategy.ruleThenAi.title')}
                  <span className="badge ok">{t('config.strategy.recommended')}</span>
                </span>
                <span className="choice-desc">
                  <Trans
                    i18nKey="config.strategy.ruleThenAi.desc"
                    components={{ rulesLink }}
                  />
                </span>
              </span>
            </label>
            <label className={`choice ${cfg.strategy === 'ai' ? 'selected' : ''}`}>
              <input
                type="radio"
                checked={cfg.strategy === 'ai'}
                onChange={() => set({ strategy: 'ai' })}
              />
              <span>
                <span className="choice-title">{t('config.strategy.ai.title')}</span>
                <span className="choice-desc">{t('config.strategy.ai.desc')}</span>
              </span>
            </label>
            <label className={`choice ${cfg.strategy === 'rule' ? 'selected' : ''}`}>
              <input
                type="radio"
                checked={cfg.strategy === 'rule'}
                onChange={() => set({ strategy: 'rule' })}
              />
              <span>
                <span className="choice-title">{t('config.strategy.rule.title')}</span>
                <span className="choice-desc">
                  <Trans i18nKey="config.strategy.rule.desc" components={{ rulesLink }} />
                </span>
              </span>
            </label>
          </div>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">{t('config.session.title')}</div>
        <div className="panel-body">
          <label className="check" style={{ marginBottom: 6 }}>
            <input
              type="checkbox"
              checked={cfg.session.sticky}
              onChange={(e) => set({ session: { ...cfg.session, sticky: e.target.checked } })}
            />
            {t('config.session.sticky')}
          </label>
          <p className="panel-note">
            <Trans i18nKey="config.session.stickyNote" components={{ code: <code /> }} />
          </p>
          <div className="row">
            <label className="field" style={{ marginBottom: 0 }}>
              <span className="field-name">
                {t('config.session.ttl')}
                <span className="field-hint">{t('config.session.ttlHint')}</span>
              </span>
              <input
                type="number"
                value={cfg.session.ttl_seconds}
                onChange={(e) => set({ session: { ...cfg.session, ttl_seconds: +e.target.value } })}
              />
            </label>
            <label className="field" style={{ marginBottom: 0 }}>
              <span className="field-name">
                {t('config.session.maxSessions')}
                <span className="field-hint">{t('config.session.maxSessionsHint')}</span>
              </span>
              <input
                type="number"
                value={cfg.session.max_sessions}
                onChange={(e) => set({ session: { ...cfg.session, max_sessions: +e.target.value } })}
              />
            </label>
          </div>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          {t('config.aiRouter.title')}
          {!aiRouterActive(cfg.strategy) && (
            <span className="badge">{t('config.aiRouter.inactive')}</span>
          )}
        </div>
        <div className="panel-body">
          <div className="row">
            <label className="field">
              <span className="field-name">
                {t('config.aiRouter.decisionModel')}
                <span className="field-hint">{t('config.aiRouter.decisionModelHint')}</span>
              </span>
              <input
                type="text"
                className="mono"
                value={cfg.ai_router.decision_model}
                onChange={(e) => set({ ai_router: { ...cfg.ai_router, decision_model: e.target.value } })}
              />
            </label>
            <label className="field">
              <span className="field-name">{t('config.aiRouter.decisionProvider')}</span>
              <select
                value={cfg.ai_router.decision_provider ?? ''}
                onChange={(e) =>
                  set({
                    ai_router: {
                      ...cfg.ai_router,
                      decision_provider: e.target.value || undefined,
                    },
                  })
                }
              >
                <option value="">
                  {t('config.models.followDefault', { name: cfg.default_provider })}
                </option>
                {providerNames.map((n) => (
                  <option key={n} value={n}>{n}</option>
                ))}
              </select>
            </label>
          </div>
          <div className="row">
            <label className="field" style={{ marginBottom: 0 }}>
              <span className="field-name">
                {t('config.aiRouter.timeout')}
                <span className="field-hint">{t('config.aiRouter.timeoutHint')}</span>
              </span>
              <input
                type="number"
                value={cfg.ai_router.timeout_seconds}
                onChange={(e) => set({ ai_router: { ...cfg.ai_router, timeout_seconds: +e.target.value } })}
              />
            </label>
            <label className="field" style={{ marginBottom: 0 }}>
              <span className="field-name">
                {t('config.aiRouter.maxPromptChars')}
                <span className="field-hint">{t('config.aiRouter.maxPromptCharsHint')}</span>
              </span>
              <input
                type="number"
                value={cfg.ai_router.max_prompt_chars}
                onChange={(e) => set({ ai_router: { ...cfg.ai_router, max_prompt_chars: +e.target.value } })}
              />
            </label>
          </div>
        </div>
      </div>

      <DecisionPromptPanel cfg={cfg} set={set} notify={notify} goto={goto} />
    </>
  )
}
