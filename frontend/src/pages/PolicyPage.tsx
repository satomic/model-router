import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { getConfig, putModelPolicyConfig, type RouterConfig } from '../api'
import { useDialogs } from '../components/Dialog'
import PolicySection from './policy/PolicySection'
import type { PolicyProps } from './policy/types'

/** The two top-level config keys this page owns. Everything else it loads is read-only context:
 *  `models` fills the group checkboxes and `auth.key_policy` decides which scopes are bindable. */
const OWNED = ['model_groups', 'model_policy'] as const

/** Key-order-independent comparison, so a mere reordering is not read as a change
 *  (same as ConfigPage and AccessPage). */
function canonical(value: unknown): string {
  return JSON.stringify(value, (_k, v) =>
    v && typeof v === 'object' && !Array.isArray(v)
      ? Object.fromEntries(Object.entries(v as object).sort(([a], [b]) => (a < b ? -1 : 1)))
      : v,
  )
}

/** Model policy: a first-level page rather than a Routing configuration tab.
 *
 *  It was a tab, and being one made it hard to find and hard to reason about -- routing decides
 *  *which* model serves a request, while this decides *which models a caller may ask for at all*.
 *  They are different jobs on different config keys, edited by different people at different times.
 *
 *  Only `model_groups` and `model_policy` are written back (`putModelPolicyConfig`), for the same
 *  reason the Access control page posts `auth` alone: the backend merges a PUT by top-level key, so
 *  a page that submitted everything it loaded could silently revert an edit saved from another page
 *  in between.
 */
export default function PolicyPage() {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  /** saved = the server's current value (advanced on a successful save), cfg = the local draft. */
  const [saved, setSaved] = useState<RouterConfig | null>(null)
  const [cfg, setCfg] = useState<RouterConfig | null>(null)
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

  // Only the owned keys are compared: a config.yaml edited elsewhere while this page was open must
  // not light the save bar up for a change this page neither made nor would submit.
  const dirtyKeys = useMemo(() => {
    if (!cfg || !saved) return new Set<string>()
    const out = new Set<string>()
    for (const k of OWNED) {
      if (canonical(cfg[k]) !== canonical(saved[k])) out.add(k)
    }
    return out
  }, [cfg, saved])

  const dirty = dirtyKeys.size > 0

  /** Intercept close/reload while changes are unsaved -- only a save writes back to config.yaml. */
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

  const props: PolicyProps = {
    cfg,
    set: (patch) => setCfg({ ...cfg, ...patch }),
    notify: (kind, msg) => setToast({ kind, msg }),
  }

  const save = async () => {
    setSaving(true)
    setToast(null)
    try {
      await putModelPolicyConfig({
        model_groups: cfg.model_groups ?? {},
        model_policy: cfg.model_policy ?? {},
      })
      // Only the owned keys advance: the rest of `saved` is still whatever was loaded, and
      // claiming otherwise would hide a later change made elsewhere behind a clean save bar.
      setSaved({ ...saved, model_groups: cfg.model_groups, model_policy: cfg.model_policy })
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
      {/* Sticky on its own -- there is no sub-nav to pair it with, this page having no sub-pages */}
      <div className="config-sticky">
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

      <PolicySection {...props} />
    </div>
  )
}
