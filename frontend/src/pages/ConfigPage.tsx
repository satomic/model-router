import { useEffect, useMemo, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import { NavLink, useNavigate, useParams } from 'react-router-dom'
import { getConfig, putConfig, type RouterConfig } from '../api'
import { useDialogs } from '../components/Dialog'
import ModelsSection from './config/ModelsSection'
import ProvidersSection from './config/ProvidersSection'
import RulesSection from './config/RulesSection'
import StrategySection from './config/StrategySection'
import type { SectionProps } from './config/types'

type SectionKey = 'providers' | 'models' | 'strategy' | 'rules'

interface SectionDef {
  key: SectionKey
  /** The config fields this sub-page owns, used to flag "unsaved" per page. */
  owns: (keyof RouterConfig)[]
  render: (p: SectionProps) => JSX.Element
}

/** Module-level, so labels and hints are resolved from `key` at render time. */
const SECTIONS: SectionDef[] = [
  {
    key: 'providers',
    owns: ['providers', 'default_provider'],
    render: (p) => <ProvidersSection {...p} />,
  },
  {
    key: 'models',
    owns: ['models'],
    render: (p) => <ModelsSection {...p} />,
  },
  {
    key: 'strategy',
    owns: ['strategy', 'session', 'ai_router'],
    render: (p) => <StrategySection {...p} />,
  },
  {
    key: 'rules',
    owns: ['rules'],
    render: (p) => <RulesSection {...p} />,
  },
]

/** What each sub-page hands off to, shown under its heading.
 *
 *  These four sub-pages are one pipeline read top to bottom -- a connection carries models, a model
 *  is chosen by a strategy, and a rule-based strategy needs rules -- but a tab strip says nothing
 *  about order or dependency. Naming the next step, and the page that decides *who may call* what
 *  is configured here, is what stops Model policy from being looked for on this page now that it
 *  has moved out.
 */
const NEXT: Record<SectionKey, { key: SectionKey | 'policy'; page?: 'policy' }> = {
  providers: { key: 'models' },
  models: { key: 'strategy' },
  strategy: { key: 'rules' },
  rules: { key: 'policy', page: 'policy' },
}

/** Key-order-independent comparison: sub-pages rebuild objects with Object.fromEntries,
 *  which can change key order, and a plain JSON.stringify would then report a mere
 *  reordering as a change. */
function canonical(value: unknown): string {
  return JSON.stringify(value, (_k, v) =>
    v && typeof v === 'object' && !Array.isArray(v)
      ? Object.fromEntries(Object.entries(v as object).sort(([a], [b]) => (a < b ? -1 : 1)))
      : v,
  )
}

export default function ConfigPage() {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  /** saved = the server's current value (advanced on a successful save), cfg = the local draft. */
  const [saved, setSaved] = useState<RouterConfig | null>(null)
  const [cfg, setCfg] = useState<RouterConfig | null>(null)
  // The sub-page comes from the URL, so /config/models is a real, shareable address. Changing
  // the :section param does not remount this component, so the draft below survives the switch.
  const { section } = useParams()
  const navigate = useNavigate()
  const [toast, setToast] = useState<{ kind: 'ok' | 'error'; msg: string } | null>(null)
  const [saving, setSaving] = useState(false)

  const load = (announce?: string) =>
    getConfig()
      .then((c) => {
        setSaved(c)
        setCfg(structuredClone(c))
        if (announce) setToast({ kind: 'ok', msg: announce })
      })
      .catch((e) => setToast({ kind: 'error', msg: String(e) }))

  useEffect(() => {
    void load()
  }, [])

  const dirtyKeys = useMemo(() => {
    if (!cfg || !saved) return new Set<string>()
    const keys = new Set([...Object.keys(cfg), ...Object.keys(saved)]) as Set<keyof RouterConfig>
    const out = new Set<string>()
    for (const k of keys) {
      if (canonical(cfg[k]) !== canonical(saved[k])) out.add(k as string)
    }
    return out
  }, [cfg, saved])

  const dirty = dirtyKeys.size > 0

  /** Intercept close/reload while changes are unsaved -- config is only written back to
   *  config.yaml on save. */
  useEffect(() => {
    if (!dirty) return
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty])

  if (!cfg || !saved) return <div className="empty">{t('config.loading')}</div>

  // A typo in the URL falls back to the first sub-page rather than rendering nothing.
  const current = SECTIONS.find((s) => s.key === section) ?? SECTIONS[0]

  const props: SectionProps = {
    cfg,
    set: (patch) => setCfg({ ...cfg, ...patch }),
    notify: (kind, msg) => setToast({ kind, msg }),
    // Same signature as before, so the four call sites in the sub-pages need no change; only
    // the destination is now a URL.
    goto: (k) => {
      navigate(`/config/${k}`)
      setToast(null)
    },
  }

  const save = async () => {
    setSaving(true)
    setToast(null)
    try {
      // Three keys this page loads but does not own are dropped rather than echoed back: `auth`
      // belongs to Access control, `model_groups`/`model_policy` to Model policy. The backend
      // merges a PUT by top-level key, so anything sent here would overwrite whatever those pages
      // saved while this one sat open -- with a value this page loaded before their edit.
      const { auth: _auth, model_groups: _g, model_policy: _p, ...rest } = cfg
      await putConfig(rest as RouterConfig)
      setSaved(structuredClone(cfg))
      setToast({ kind: 'ok', msg: t('config.saved') })
    } catch (e) {
      setToast({ kind: 'error', msg: String(e) })
    } finally {
      setSaving(false)
    }
  }

  const discard = async () => {
    if (
      dirty &&
      !(await dialogs.confirm({
        title: t('common.discardChanges'),
        message: t('common.confirmDiscard'),
        confirmLabel: t('common.discardChanges'),
        danger: true,
      }))
    )
      return
    void load(t('common.reloadedFromFile'))
  }

  return (
    <div>
      {/* The sub-nav and the save bar are sticky as one block, so the current sub-page and
          the save button stay visible however far the page is scrolled */}
      <div className="config-sticky">
        <nav className="subnav">
          {SECTIONS.map((s) => {
            const sectionDirty = s.owns.some((k) => dirtyKeys.has(k as string))
            return (
              // `current.key` rather than isActive, so the fallback sub-page is the one shown
              // as active when the URL names a section that does not exist.
              <NavLink
                key={s.key}
                to={`/config/${s.key}`}
                className={`subnav-item ${current.key === s.key ? 'active' : ''}`}
                title={t(`config.section.${s.key}.hint`)}
                onClick={() => setToast(null)}
              >
                {t(`config.section.${s.key}.label`)}
                {sectionDirty && (
                  <span className="dirty-dot" title={t('common.hasUnsavedChanges')} />
                )}
              </NavLink>
            )
          })}
        </nav>

        {/* The whole bar takes the accent colour once anything changed, so edits are not
            left unsaved by accident */}
        <div className={`savebar ${dirty ? 'dirty' : ''}`}>
          <span className="state">
            {dirty ? (
              <>
                <span className="dirty-dot" />
                {t('common.hasUnsavedChanges')}
                <span className="dim">{t('common.dirtySections', { count: dirtyKeys.size })}</span>
              </>
            ) : (
              <span className="dim">{t('common.inSyncWithFile')}</span>
            )}
          </span>
          <span className="spacer" />
          <button className="btn ghost" onClick={discard} disabled={saving}>
            {dirty ? t('common.discardChanges') : t('common.reload')}
          </button>
          <button className="btn" onClick={save} disabled={saving || !dirty}>
            {saving ? t('common.saving') : t('common.saveAndApply')}
          </button>
        </div>
      </div>

      {toast && <div className={`toast ${toast.kind}`}>{toast.msg}</div>}

      <div className="section-intro">
        <h2>
          {t('config.step', {
            n: SECTIONS.indexOf(current) + 1,
            total: SECTIONS.length,
            label: t(`config.section.${current.key}.label`),
          })}
        </h2>
        <span className="dim">{t(`config.section.${current.key}.hint`)}</span>
        <p className="section-guide">
          <Trans
            i18nKey={`config.guide.${current.key}`}
            components={{ strong: <strong />, code: <code /> }}
          />{' '}
          {/* A link rather than prose: the next step is one click away, and the last step's
              next stop is a different page entirely. */}
          <button
            className="btn-link"
            onClick={() => {
              const next = NEXT[current.key]
              if (next.page) navigate(`/${next.page}`)
              else props.goto(next.key as SectionKey)
            }}
          >
            {t('config.nextStep', {
              label: t(
                NEXT[current.key].page
                  ? `nav.${NEXT[current.key].key}.label`
                  : `config.section.${NEXT[current.key].key}.label`,
              ),
            })}
          </button>
        </p>
      </div>

      {current.render(props)}
    </div>
  )
}
