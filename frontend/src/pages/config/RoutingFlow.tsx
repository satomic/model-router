import { useTranslation } from 'react-i18next'
import type { RouterConfig } from '../../api'
import { aiRouterActive, rulesActive } from './strategy'

/**
 * The routing chain the current strategy produces, as four stages a request passes through.
 *
 * All four are always drawn and the ones the selected strategy does not consult are dimmed and
 * marked, rather than being hidden: the difference between the three strategies *is* which
 * stages are live, and a picture that changes shape as the radio moves teaches that in a way
 * three paragraphs of prose next to three radio buttons do not. Clicking the rules or decision
 * stage goes to the page that configures it.
 */
export default function RoutingFlow({
  cfg,
  goto,
}: {
  cfg: RouterConfig
  goto: (section: string) => void
}) {
  const { t } = useTranslation()
  const usesRules = rulesActive(cfg.strategy)
  const usesAi = aiRouterActive(cfg.strategy)
  const defaultModel = Object.keys(cfg.models).find((n) => cfg.models[n].default)
  const notSet = t('config.flow.noModel')

  const stages = [
    {
      key: 'request',
      on: true,
      name: t('config.flow.request'),
      value: t('config.flow.requestNote'),
      mono: true,
    },
    {
      key: 'rules',
      on: usesRules,
      name: t('config.flow.rules'),
      value: t('config.rules.count', { count: cfg.rules.length }),
      note: t('config.flow.rulesNote'),
      go: 'rules',
    },
    {
      key: 'ai',
      on: usesAi,
      name: t('config.flow.ai'),
      value: cfg.ai_router.decision_model || notSet,
      note: t('config.flow.aiNote'),
      mono: true,
    },
    {
      key: 'default',
      on: true,
      name: t('config.flow.default'),
      value: defaultModel ?? notSet,
      note: t('config.flow.defaultNote'),
      go: 'models',
      mono: true,
    },
  ]

  return (
    <div className="panel">
      <div className="panel-head">{t('config.flow.title')}</div>
      <div className="panel-body">
        <div className="flow">
          {stages.map((s, i) => (
            <div className="flow-step" key={s.key}>
              {i > 0 && <span className="flow-arrow">→</span>}
              <div
                className={`flow-stage ${s.on ? 'on' : 'off'} ${s.go ? 'linked' : ''}`}
                onClick={s.go ? () => goto(s.go as string) : undefined}
              >
                <span className="flow-name">
                  {s.name}
                  {!s.on && <span className="badge">{t('config.flow.unused')}</span>}
                </span>
                <span className={`flow-value ${s.mono ? 'mono' : ''}`}>{s.value}</span>
                {s.note && <span className="flow-note">{s.note}</span>}
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}
