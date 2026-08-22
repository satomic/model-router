import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { NavLink, useParams } from 'react-router-dom'
import { getConfig, putAuthConfig, type AuthConfig } from '../api'
import { useDialogs } from '../components/Dialog'
import AdminsSection from './access/AdminsSection'
import KeyPolicySection from './access/KeyPolicySection'
import KeyScopeSection from './access/KeyScopeSection'
import LocalAdminSection from './access/LocalAdminSection'
import OAuthSection from './access/OAuthSection'
import type { AccessSectionProps } from './access/types'

type SectionKey = 'admins' | 'oauth' | 'local' | 'policy' | 'keyscope'

interface SectionDef {
  key: SectionKey
  /** The auth fields this sub-page owns, used to flag "unsaved" per page. */
  owns: (keyof AuthConfig)[]
  render: (p: AccessSectionProps) => JSX.Element
}

/** Module-level, so labels and hints are resolved from `key` at render time. */
const SECTIONS: SectionDef[] = [
  {
    key: 'admins',
    owns: ['admin_logins', 'allow_any_github_user'],
    render: (p) => <AdminsSection {...p} />,
  },
  {
    key: 'oauth',
    owns: ['github'],
    render: (p) => <OAuthSection {...p} />,
  },
  {
    // owns: [] on purpose -- this section writes through /v1/auth/local/* immediately rather than
    // through the shared draft, so it has nothing for the dirty dot to track.
    key: 'local',
    owns: [],
    render: (p) => <LocalAdminSection {...p} />,
  },
  {
    key: 'policy',
    owns: ['key_policy'],
    render: (p) => <KeyPolicySection {...p} />,
  },
  {
    key: 'keyscope',
    owns: ['key_scope_policy'],
    render: (p) => <KeyScopeSection {...p} />,
  },
]

/** Key-order-independent comparison, so a mere reordering is not read as a change
 *  (same as ConfigPage). */
function canonical(value: unknown): string {
  return JSON.stringify(value, (_k, v) =>
    v && typeof v === 'object' && !Array.isArray(v)
      ? Object.fromEntries(Object.entries(v as object).sort(([a], [b]) => (a < b ? -1 : 1)))
      : v,
  )
}

const EMPTY: AuthConfig = { github: {} }

/** The sub-page /access lands on. Deliberately not SECTIONS[0]: the key policy is what an
 *  administrator comes here for, while SECTIONS order is the visible tab order. */
const DEFAULT_SECTION: SectionKey = 'policy'

/** Access control: who may sign in, who is an administrator, and who may create API keys
 *  (hence who can use BYOK).
 *
 *  Only the auth section of config.yaml is written back (putAuthConfig merges by top-level
 *  key), so this page and the Routing configuration page save independently of each other.
 */
export default function AccessPage() {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  const [saved, setSaved] = useState<AuthConfig | null>(null)
  const [auth, setAuth] = useState<AuthConfig | null>(null)
  // From the URL, so /access/policy is a real address (see ConfigPage for the same pattern).
  const { section } = useParams()
  const [toast, setToast] = useState<{ kind: 'ok' | 'error'; msg: string } | null>(null)
  const [saving, setSaving] = useState(false)

  const load = (announce?: string) =>
    getConfig()
      .then((c) => {
        const next = c.auth ?? EMPTY
        setSaved(next)
        setAuth(structuredClone(next))
        if (announce) setToast({ kind: 'ok', msg: announce })
      })
      .catch((e) => setToast({ kind: 'error', msg: String(e) }))

  useEffect(() => {
    void load()
  }, [])

  const dirtyKeys = useMemo(() => {
    if (!auth || !saved) return new Set<string>()
    const keys = new Set([...Object.keys(auth), ...Object.keys(saved)]) as Set<keyof AuthConfig>
    const out = new Set<string>()
    for (const k of keys) {
      if (canonical(auth[k]) !== canonical(saved[k])) out.add(k as string)
    }
    return out
  }, [auth, saved])

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

  if (!auth || !saved) return <div className="empty">{t('access.loading')}</div>

  const current =
    SECTIONS.find((s) => s.key === section) ??
    SECTIONS.find((s) => s.key === DEFAULT_SECTION)!

  const props: AccessSectionProps = {
    auth,
    saved,
    set: (patch) => setAuth({ ...auth, ...patch }),
    notify: (kind, msg) => setToast({ kind, msg }),
  }

  const save = async () => {
    setSaving(true)
    setToast(null)
    try {
      await putAuthConfig(auth)
      setSaved(structuredClone(auth))
      setToast({ kind: 'ok', msg: t('access.saved') })
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
      {/* The sub-nav and the save bar are sticky as one block (same as Routing configuration) */}
      <div className="config-sticky">
        <nav className="subnav">
          {SECTIONS.map((s) => {
            const sectionDirty = s.owns.some((k) => dirtyKeys.has(k as string))
            return (
              <NavLink
                key={s.key}
                to={`/access/${s.key}`}
                className={`subnav-item ${current.key === s.key ? 'active' : ''}`}
                title={t(`access.section.${s.key}.hint`)}
                onClick={() => setToast(null)}
              >
                {t(`access.section.${s.key}.label`)}
                {sectionDirty && (
                  <span className="dirty-dot" title={t('common.hasUnsavedChanges')} />
                )}
              </NavLink>
            )
          })}
        </nav>

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
        <h2>{t(`access.section.${current.key}.label`)}</h2>
        <span className="dim">{t(`access.section.${current.key}.hint`)}</span>
      </div>

      {current.render(props)}
    </div>
  )
}
