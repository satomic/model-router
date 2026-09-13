import { useEffect, useMemo, useRef, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import {
  discoverEnterprises,
  getConfig,
  getCredits,
  previewSchedule,
  putAICreditsConfig,
  refreshCredits,
  type AICreditsConfig,
  type CreditsEnterprise,
  type CreditsStatus,
  type DiscoveredEnterprise,
} from '../api'
import { useDialogs } from '../components/Dialog'
import { formatDateTime } from '../i18n/format'
import './credits/credits.css'

/** The one top-level config key this page owns. */
const DEFAULT_SCHEDULE = '*/30 * * * *'

const PRESETS: { label: string; expr: string }[] = [
  { label: '15m', expr: '*/15 * * * *' },
  { label: '30m', expr: '*/30 * * * *' },
  { label: '1h', expr: '0 * * * *' },
  { label: '6h', expr: '0 */6 * * *' },
  { label: 'daily', expr: '0 1 * * *' },
]

function canonical(value: unknown): string {
  return JSON.stringify(value, (_k, v) =>
    v && typeof v === 'object' && !Array.isArray(v)
      ? Object.fromEntries(Object.entries(v as object).sort(([a], [b]) => (a < b ? -1 : 1)))
      : v,
  )
}

const fmtCredits = (n: number | null | undefined) =>
  n === null || n === undefined ? '—' : Math.round(n).toLocaleString()
const fmtUsd = (n: number | null | undefined) =>
  n === null || n === undefined ? '—' : `$${n.toFixed(2)}`

/** Copilot AI credits: what the enterprise pool looks like right now, how often to ask GitHub,
 *  and whether BYOK is held back while the pool still has credits.
 *
 *  Only `ai_credits` is written back (`putAICreditsConfig`): the backend merges a PUT by
 *  top-level key, and this page must not carry -- and so cannot stale-overwrite -- anything the
 *  other configuration pages own.
 */
export default function CreditsPage() {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  const [saved, setSaved] = useState<AICreditsConfig | null>(null)
  const [draft, setDraft] = useState<AICreditsConfig | null>(null)
  const [status, setStatus] = useState<CreditsStatus | null>(null)
  const [known, setKnown] = useState<DiscoveredEnterprise[]>([])
  const [toast, setToast] = useState<{ kind: 'ok' | 'error'; msg: string } | null>(null)
  const [saving, setSaving] = useState(false)
  const [refreshing, setRefreshing] = useState(false)

  const loadStatus = () => getCredits().then(setStatus).catch((e) => setToast({ kind: 'error', msg: String(e) }))

  const load = (announce?: string) =>
    getConfig()
      .then((c) => {
        const next = c.ai_credits ?? {}
        setSaved(next)
        setDraft(structuredClone(next))
        if (announce) setToast({ kind: 'ok', msg: announce })
      })
      .catch((e) => setToast({ kind: 'error', msg: String(e) }))

  useEffect(() => {
    void load()
    void loadStatus()
    discoverEnterprises()
      .then((r) => setKnown(r.enterprises))
      .catch(() => setKnown([]))
  }, [])

  // While a poll is due, keep asking: the scheduler fires within 30 s, and the page should show
  // the figures land without a manual reload.
  useEffect(() => {
    if (!status?.due) return
    const id = setInterval(loadStatus, 5000)
    return () => clearInterval(id)
  }, [status?.due])

  const dirty = useMemo(() => canonical(draft) !== canonical(saved), [draft, saved])

  useEffect(() => {
    if (!dirty) return
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty])

  if (!draft || !saved) return <div className="empty">{t('config.loading')}</div>

  const set = (patch: Partial<AICreditsConfig>) => setDraft({ ...draft, ...patch })
  const setGate = (patch: Partial<NonNullable<AICreditsConfig['gate']>>) =>
    set({ gate: { ...(draft.gate ?? {}), ...patch } })

  const save = async () => {
    setSaving(true)
    setToast(null)
    try {
      await putAICreditsConfig(draft)
      setSaved(structuredClone(draft))
      setToast({ kind: 'ok', msg: t('config.saved') })
      void loadStatus()
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

  const refresh = async () => {
    setRefreshing(true)
    setToast(null)
    try {
      setStatus(await refreshCredits())
      setToast({ kind: 'ok', msg: t('credits.refreshed') })
    } catch (e) {
      setToast({ kind: 'error', msg: String(e) })
    } finally {
      setRefreshing(false)
    }
  }

  const gate = draft.gate ?? {}
  const gateOn = Boolean(gate.enabled)
  const pollOn = Boolean(draft.enabled)
  const selected = draft.enterprises ?? []
  // Slugs to offer: what discovery saw plus whatever the last poll returned, so the list is not
  // empty on a deployment whose structure cache has not been built yet.
  const slugs = Array.from(
    new Set([
      ...known.map((e) => e.slug),
      ...(status?.enterprises ?? []).map((e) => e.slug),
      ...selected,
    ]),
  ).sort()
  const nameOf = (slug: string) =>
    known.find((e) => e.slug === slug)?.name ?? status?.enterprises.find((e) => e.slug === slug)?.name ?? slug

  return (
    <div>
      <div className="config-sticky">
        <div className={`savebar ${dirty ? 'dirty' : ''}`}>
          <span className="state">
            {dirty ? (
              <>
                <span className="dirty-dot" />
                {t('common.hasUnsavedChanges')}
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

      {status && !status.token_configured && (
        <div className="toast warn">
          <Trans i18nKey="credits.noToken" components={{ strong: <strong /> }} />
        </div>
      )}

      {/* -- Pool state -- */}
      <div className="panel">
        <div className="panel-head">
          {t('credits.poolTitle')}
          {status?.fetched_at ? (
            <span className={`badge ${status.stale ? 'warn' : 'ok'}`}>
              {status.stale ? t('credits.stale') : t('credits.fresh')}
            </span>
          ) : (
            <span className="badge warn">{t('credits.neverFetched')}</span>
          )}
          <span className="spacer" />
          <button className="btn ghost sm" onClick={refresh} disabled={refreshing || !status?.token_configured}>
            {refreshing ? t('credits.refreshing') : t('credits.refreshNow')}
          </button>
        </div>
        <div className="panel-body">
          <p className="panel-note" style={{ marginTop: 0 }}>
            <Trans i18nKey="credits.poolLead" components={{ strong: <strong /> }} />
          </p>
          <div className="credits-runs">
            <span>
              {t('credits.lastFetched')}: <b>{status?.fetched_at ? formatDateTime(status.fetched_at * 1000) : '—'}</b>
            </span>
            <span>
              {t('credits.nextRun')}:{' '}
              <b>
                {!status?.settings.enabled
                  ? t('credits.pollingOff')
                  : status.due
                    ? t('credits.dueNow')
                    : status.next_run_at
                      ? formatDateTime(status.next_run_at * 1000)
                      : '—'}
              </b>
            </span>
            {status?.token_changed && <span className="credits-warn">{t('credits.tokenChanged')}</span>}
            {(status?.missing_enterprises.length ?? 0) > 0 && (
              <span className="credits-warn">
                {t('credits.missingEnterprises', { list: status!.missing_enterprises.join(', ') })}
              </span>
            )}
          </div>

          {status && status.enterprises.length === 0 && status.fetched_at && (
            <div className="empty">{t('credits.noEnterprises')}</div>
          )}
          {status?.enterprises.map((e) => (
            <EnterpriseCard key={e.slug} ent={e} perUser={status.snapshot_per_user} />
          ))}
        </div>
      </div>

      {/* -- Polling schedule -- */}
      <div className="panel">
        <div className="panel-head">
          {t('credits.scheduleTitle')}
          {pollOn ? (
            <span className="badge ok">{t('credits.on')}</span>
          ) : (
            <span className="badge warn">{t('credits.off')}</span>
          )}
        </div>
        <div className="panel-body">
          <label className="check">
            <input type="checkbox" checked={pollOn} onChange={(e) => set({ enabled: e.target.checked })} />
            {t('credits.pollToggle')}
          </label>
          <p className="panel-note" style={{ marginTop: 10 }}>
            <Trans i18nKey="credits.pollNote" components={{ strong: <strong />, code: <code /> }} />
          </p>

          <div className="credits-form">
            <ScheduleField
              value={draft.schedule ?? DEFAULT_SCHEDULE}
              onChange={(schedule) => set({ schedule })}
            />

            <div className="field">
              <span className="field-name">
                {t('credits.enterprises')}
                <span className="field-hint">{t('credits.enterprisesHint')}</span>
              </span>
              {slugs.length === 0 ? (
                <div className="dim" style={{ fontSize: 12.5 }}>
                  {t('credits.noKnownEnterprises')}
                </div>
              ) : (
                <div className="credits-ent-list">
                  {slugs.map((slug) => (
                    <label key={slug} className="check">
                      <input
                        type="checkbox"
                        checked={selected.includes(slug)}
                        onChange={(ev) =>
                          set({
                            enterprises: ev.target.checked
                              ? [...selected, slug]
                              : selected.filter((s) => s !== slug),
                          })
                        }
                      />
                      <span className="credits-ent-option">
                        <span>{nameOf(slug)}</span>
                        <span className="mono dim">{slug}</span>
                      </span>
                    </label>
                  ))}
                </div>
              )}
              <p className="panel-note credits-form-foot">
                {selected.length === 0 ? t('credits.allEnterprises') : t('credits.someEnterprises', { count: selected.length })}
              </p>
            </div>
          </div>
        </div>
      </div>

      {/* -- The gate -- */}
      <div className="panel">
        <div className="panel-head">
          {t('credits.gateTitle')}
          {gateOn ? (
            <span className="badge ok">{t('credits.gateOn')}</span>
          ) : (
            <span className="badge warn">{t('credits.off')}</span>
          )}
        </div>
        <div className="panel-body">
          <label className="check">
            <input type="checkbox" checked={gateOn} onChange={(e) => setGate({ enabled: e.target.checked })} />
            {t('credits.gateToggle')}
          </label>
          <p className="panel-note" style={{ marginTop: 10 }}>
            <Trans
              i18nKey={gateOn ? 'credits.gateOnNote' : 'credits.gateOffNote'}
              components={{ strong: <strong /> }}
            />
          </p>
          {gateOn && !pollOn && <div className="toast warn">{t('credits.gateNeedsPolling')}</div>}

          <label className="check">
            <input
              type="checkbox"
              checked={Boolean(gate.per_user)}
              onChange={(e) => setGate({ per_user: e.target.checked })}
            />
            {t('credits.perUserToggle')}
          </label>
          <p className="panel-note" style={{ marginTop: 10 }}>
            <Trans i18nKey="credits.perUserNote" components={{ strong: <strong /> }} />
          </p>

          <label className="field">
            <span className="field-name">
              {t('credits.minRemaining')}
              <span className="field-hint">{t('credits.minRemainingHint')}</span>
            </span>
            <input
              type="number"
              min={0}
              step={100}
              style={{ maxWidth: 200 }}
              value={draft.min_remaining_credits ?? 0}
              onChange={(e) => set({ min_remaining_credits: Math.max(0, Number(e.target.value) || 0) })}
            />
          </label>

          <label className="field">
            <span className="field-name">
              {t('credits.message')}
              <span className="field-hint">{t('credits.messageHint')}</span>
              <span className="spacer" />
              {(gate.message ?? '').trim() && (
                <button className="btn-link" type="button" onClick={() => setGate({ message: '' })}>
                  {t('credits.restoreDefault')}
                </button>
              )}
            </span>
            <textarea
              rows={4}
              value={gate.message ?? ''}
              placeholder={status?.default_message ?? ''}
              onChange={(e) => setGate({ message: e.target.value })}
            />
          </label>
          <p className="panel-note" style={{ margin: '-6px 0 14px' }}>
            <Trans i18nKey="credits.messagePlaceholders" components={{ code: <code /> }} />
          </p>

          {gate.per_user && (
            <>
              <label className="field" style={{ marginBottom: 0 }}>
                <span className="field-name">
                  {t('credits.messageBudget')}
                  <span className="field-hint">{t('credits.messageHint')}</span>
                  <span className="spacer" />
                  {(gate.message_budget ?? '').trim() && (
                    <button className="btn-link" type="button" onClick={() => setGate({ message_budget: '' })}>
                      {t('credits.restoreDefault')}
                    </button>
                  )}
                </span>
                <textarea
                  rows={4}
                  value={gate.message_budget ?? ''}
                  placeholder={status?.default_message_budget ?? ''}
                  onChange={(e) => setGate({ message_budget: e.target.value })}
                />
              </label>
              <p className="panel-note" style={{ margin: '6px 0 0' }}>
                <Trans i18nKey="credits.messageBudgetPlaceholders" components={{ code: <code /> }} />
              </p>
            </>
          )}
        </div>
      </div>
    </div>
  )
}

/** Cron input with presets and a server-rendered preview of the next firings -- the backend
 *  owns the parser, so what is previewed is exactly what will run. */
function ScheduleField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const { t } = useTranslation()
  const [preview, setPreview] = useState<{ valid: boolean; error: string | null; next: number[] } | null>(null)
  const timer = useRef<number | undefined>(undefined)

  useEffect(() => {
    const ctl = new AbortController()
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => {
      previewSchedule(value, ctl.signal)
        .then(setPreview)
        .catch(() => undefined)
    }, 250)
    return () => {
      ctl.abort()
      window.clearTimeout(timer.current)
    }
  }, [value])

  return (
    <div className="field">
      <span className="field-name">
        {t('credits.schedule')}
        <span className="field-hint">{t('credits.scheduleHint')}</span>
      </span>
      <input
        className="mono credits-cron"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        spellCheck={false}
      />
      <div className="credits-presets">
        {PRESETS.map((p) => (
          <button
            key={p.expr}
            type="button"
            className={`btn ghost sm ${value.trim() === p.expr ? 'active' : ''}`}
            onClick={() => onChange(p.expr)}
          >
            {t(`credits.preset.${p.label}`)}
          </button>
        ))}
      </div>
      {preview && !preview.valid && <p className="credits-err">{t('credits.badSchedule', { error: preview.error })}</p>}
      {preview?.valid && (
        <div className="credits-next">
          <span className="credits-next-label">{t('credits.nextRuns')}</span>
          <ol>
            {preview.next.map((ts) => (
              <li key={ts}>{formatDateTime(ts * 1000)}</li>
            ))}
          </ol>
        </div>
      )}
    </div>
  )
}

function EnterpriseCard({ ent, perUser }: { ent: CreditsEnterprise; perUser: boolean }) {
  const { t } = useTranslation()
  const total = ent.pool_total
  const pct = total && total > 0 ? Math.min(100, (ent.consumed / total) * 100) : null
  const meteredPct = total && total > 0 ? Math.min(100 - (pct ?? 0), (ent.metered / total) * 100) : 0
  const stateBadge =
    ent.error ? 'error' : ent.state === 'available' ? 'ok' : ent.state === 'exhausted' ? 'warn' : ''

  return (
    <div className="provider-card">
      <div className="credits-ent-head">
        <span className="name">{ent.name}</span>
        <span className="mono dim">{ent.slug}</span>
        <span className={`badge ${stateBadge}`}>
          {ent.error ? t('credits.state.error') : t(`credits.state.${ent.state}`)}
        </span>
        {ent.pool_total_source && (
          <span className="badge">{t(`credits.source.${ent.pool_total_source}`)}</span>
        )}
        {!ent.error && (
          <span className="badge" title={t('credits.memberSourceHint')}>
            {ent.member_source
              ? t(`credits.memberSource.${ent.member_source}`, { count: ent.member_count })
              : t('credits.memberSource.none')}
          </span>
        )}
      </div>
      {ent.error && <p className="credits-err">{ent.error}</p>}
      {!ent.error && (
        <>
          <div className={`credits-meter ${ent.state === 'exhausted' ? 'exhausted' : ''}`} style={{ marginTop: 10 }}>
            {pct !== null && <span className="covered" style={{ width: `${pct}%` }} />}
            {meteredPct > 0 && <span className="metered" style={{ width: `${meteredPct}%` }} />}
          </div>
          <div className="credits-figures">
            <span>
              {t('credits.poolTotal')}: <b>{fmtCredits(total)}</b>
            </span>
            <span>
              {t('credits.consumed')}: <b>{fmtCredits(ent.consumed)}</b>
            </span>
            <span>
              {t('credits.remaining')}: <b>{fmtCredits(ent.remaining)}</b>
              {pct !== null && <span className="dim"> ({Math.round(100 - pct)}%)</span>}
            </span>
            {ent.metered > 0 && (
              <span>
                {t('credits.metered')}: <b>{fmtCredits(ent.metered)}</b>
                <span className="dim"> (≈ ${(ent.metered * 0.01).toFixed(2)})</span>
              </span>
            )}
            {ent.seats && (
              <span>
                {t('credits.seats')}:{' '}
                <b>
                  {Object.entries(ent.seats)
                    .map(([plan, n]) => `${n} ${t(`credits.plan.${plan}`, { defaultValue: plan })}`)
                    .join(' · ') || '0'}
                </b>
              </span>
            )}
            {ent.universal_budget_usd !== null && (
              <span>
                {t('credits.universalBudget')}: <b>{fmtUsd(ent.universal_budget_usd)}</b>
              </span>
            )}
          </div>
          {ent.skus.length > 0 && (
            <div className="credits-sku">
              {ent.skus.map((s) => (
                <span key={s.sku} style={{ marginRight: 14 }}>
                  <span className="mono">{s.sku}</span> {fmtCredits(s.covered)}
                  {s.metered > 0 && ` + ${fmtCredits(s.metered)} ${t('credits.meteredShort')}`}
                </span>
              ))}
            </div>
          )}
          {ent.warnings.map((w) => (
            <p key={w} className="credits-warn">
              ⚠ {w}
            </p>
          ))}
          {perUser && <UserTable ent={ent} />}
        </>
      )}
    </div>
  )
}

function UserTable({ ent }: { ent: CreditsEnterprise }) {
  const { t } = useTranslation()
  const [filter, setFilter] = useState('')
  const [limit, setLimit] = useState(25)
  const rows = useMemo(() => {
    const q = filter.trim().toLowerCase()
    return q ? ent.users.filter((u) => u.login.includes(q)) : ent.users
  }, [ent.users, filter])

  if (ent.users.length === 0) {
    return <p className="panel-note" style={{ marginTop: 10 }}>{t('credits.noUsers')}</p>
  }
  return (
    <div className="credits-users">
      <div className="toolbar">
        <span className="badge">{t('credits.userCount', { count: ent.users.length })}</span>
        <input
          placeholder={t('credits.filterUsers')}
          value={filter}
          onChange={(e) => {
            setFilter(e.target.value)
            setLimit(25)
          }}
        />
      </div>
      <div className="table-scroll">
        <table>
          <thead>
            <tr>
              <th>{t('credits.table.login')}</th>
              <th>{t('credits.table.plan')}</th>
              <th className="num">{t('credits.table.budget')}</th>
              <th className="num">{t('credits.table.consumed')}</th>
              <th className="num">{t('credits.table.headroom')}</th>
              <th>{t('credits.table.verdict')}</th>
            </tr>
          </thead>
          <tbody>
            {rows.slice(0, limit).map((u) => (
              <tr key={u.login}>
                <td className="mono">{u.login}</td>
                <td>{u.plan ? t(`credits.plan.${u.plan}`, { defaultValue: u.plan }) : '—'}</td>
                <td className="num">{fmtUsd(u.target_usd)}</td>
                <td className="num">
                  {fmtUsd(u.consumed_usd)}
                  {u.consumed_usd !== null && (
                    <span className="dim"> ({fmtCredits(u.consumed_usd * 100)})</span>
                  )}
                </td>
                <td className="num">{u.headroom_usd === null ? t('credits.noBudget') : fmtUsd(u.headroom_usd)}</td>
                <td>
                  {u.gate === 'budget' ? (
                    <span className="badge ok">{t('credits.verdict.useCopilotBudget')}</span>
                  ) : u.gate === 'pool' ? (
                    <span className="badge ok">{t('credits.verdict.useCopilot')}</span>
                  ) : u.blocked_on_copilot ? (
                    <span className="badge warn">{t('credits.verdict.blockedOnCopilot')}</span>
                  ) : (
                    <span className="badge">{t('credits.verdict.byokOpen')}</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {rows.length > limit && (
        <button className="btn-link" type="button" onClick={() => setLimit((n) => n + 50)}>
          {t('credits.showMore', { count: rows.length - limit })}
        </button>
      )}
    </div>
  )
}
