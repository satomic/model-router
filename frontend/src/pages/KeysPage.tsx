import { useCallback, useEffect, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import {
  createKey,
  deleteKey,
  getKeys,
  getMyAccess,
  setKeyDisabled,
  type AccessVerdict,
  type ApiKey,
  type SessionUser,
} from '../api'
import { formatDateTime } from '../i18n/format'

/** Unix seconds -> a locale-formatted timestamp, or an em dash when never used. */
const fmt = (ts: number | null) => (ts ? formatDateTime(ts * 1000) : '—')

/** How much of a key stays visible while it is masked -- the same 12 characters the backend
 *  puts in `prefix`, so masked and unavailable rows line up. */
const mask = (prefix: string) => `${prefix}${'•'.repeat(12)}`

/**
 * One clipboard helper for the whole page.
 *
 * `copiedId` rather than a boolean, so the confirmation appears on the row that was actually
 * copied. The Clipboard API rejects outright on a non-secure origin (plain http on anything
 * but localhost), which is a realistic way to run this console -- so the failure is surfaced
 * instead of leaving a button that silently does nothing.
 */
function useCopy(): {
  copiedId: string | null
  copyError: string
  copy: (id: string, text: string) => void
} {
  const { t } = useTranslation()
  const [copiedId, setCopiedId] = useState<string | null>(null)
  const [copyError, setCopyError] = useState('')

  useEffect(() => {
    if (!copiedId) return
    const timer = setTimeout(() => setCopiedId(null), 2000)
    return () => clearTimeout(timer)
  }, [copiedId])

  const copy = (id: string, text: string) => {
    setCopyError('')
    navigator.clipboard
      ?.writeText(text)
      .then(() => setCopiedId(id))
      .catch(() => setCopyError(t('keys.copyFailed')))
  }
  return { copiedId, copyError, copy }
}

/** API key management: a key stays readable by its owner, with a Copilot BYOK snippet.
 *
 *  Whether a user may create a key is configured by an administrator under
 *  "Access control -> Key policy" per Enterprise / Team / Organization, and enforced by
 *  the backend on POST /v1/keys. This page asks /v1/access/me up front and lays the
 *  verdict and its evidence out explicitly -- otherwise the user just gets a 403 with no
 *  idea whom to ask for access.
 */
export default function KeysPage({ user }: { user: SessionUser }) {
  const { t } = useTranslation()
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [showAll, setShowAll] = useState(false)
  const [name, setName] = useState('')
  const [fresh, setFresh] = useState<ApiKey | null>(null)
  /** At most one key is unmasked at a time, so a screen-share does not expose the whole table. */
  const [revealedId, setRevealedId] = useState<string | null>(null)
  const { copiedId, copyError, copy } = useCopy()
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [access, setAccess] = useState<AccessVerdict | null>(null)
  const [accessError, setAccessError] = useState('')
  const [showDetail, setShowDetail] = useState(false)

  const load = useCallback(
    () =>
      getKeys(showAll && user.is_admin)
        .then(setKeys)
        .catch((e) => setError(String(e))),
    [showAll, user.is_admin],
  )

  useEffect(() => {
    void load()
  }, [load])

  /** Every verdict asks GitHub live, which can take a few hundred ms, so it loads in
   *  parallel with the key list rather than blocking it. */
  const loadAccess = useCallback(() => {
    setAccessError('')
    getMyAccess()
      .then(setAccess)
      .catch((e) => {
        setAccess(null)
        setAccessError(String(e))
      })
  }, [])

  useEffect(() => {
    loadAccess()
  }, [loadAccess])

  const create = async () => {
    setBusy(true)
    setError('')
    try {
      const created = await createKey(name.trim() || 'default')
      setFresh(created)
      setName('')
      await load()
    } catch (e) {
      setError(String(e))
      // A 403 usually means membership just changed (or the policy did), so re-fetch the
      // verdict to keep this page's explanation consistent with the backend
      loadAccess()
    } finally {
      setBusy(false)
    }
  }

  const origin = window.location.origin
  /** The button is not disabled while the verdict is still loading: the backend enforces
   *  the rule anyway, so one slow request should not lock up the UI. */
  const blocked = access !== null && !access.allowed

  return (
    <div>
      {error && <div className="toast error">{error}</div>}
      {copyError && <div className="toast error">{copyError}</div>}

      <AccessPanel
        access={access}
        error={accessError}
        showDetail={showDetail}
        onToggleDetail={() => setShowDetail((v) => !v)}
        onRefresh={loadAccess}
      />

      {fresh?.key && (
        <div className="panel">
          <div className="panel-head">
            {t('keys.created.title')}
            <span className="badge ok">{t('keys.created.readyBadge')}</span>
            <span className="spacer" />
            <button className="btn subtle sm" onClick={() => setFresh(null)}>
              {t('common.close')}
            </button>
          </div>
          <div className="panel-body">
            <p className="panel-note">{t('keys.created.note')}</p>
            <div className="secret-reveal">
              <code>{fresh.key}</code>
              <button className="btn ghost sm" onClick={() => copy('fresh', fresh.key!)}>
                {copiedId === 'fresh' ? t('common.copied') : t('common.copy')}
              </button>
            </div>
            <div style={{ marginTop: 14 }}>
              <div className="field-name">{t('keys.created.byokTitle')}</div>
              <pre className="code">{`Base URL : ${origin}/v1
API Key  : ${fresh.key}
Model    : ${t('keys.created.modelHint')}

# ${t('keys.created.curlComment')}
curl ${origin}/v1/chat/completions \\
  -H "Authorization: Bearer ${fresh.key}" \\
  -H "Content-Type: application/json" \\
  -d '{"messages":[{"role":"user","content":"hello"}]}'`}</pre>
              <p className="panel-note" style={{ marginTop: 10, marginBottom: 0 }}>
                <Trans
                  i18nKey="keys.created.attribution"
                  values={{ login: fresh.user_login }}
                  components={{ code: <code /> }}
                />
              </p>
            </div>
          </div>
        </div>
      )}

      <div className="panel">
        <div className="panel-head">
          {t('keys.create.title')}
          {blocked && <span className="badge error">{t('keys.create.notAllowedBadge')}</span>}
        </div>
        <div className="panel-body">
          <div className="row" style={{ alignItems: 'flex-end' }}>
            <label className="field" style={{ marginBottom: 0, flex: 3 }}>
              <span className="field-name">
                {t('keys.create.name')}
                <span className="field-hint">{t('keys.create.nameHint')}</span>
              </span>
              <input
                type="text"
                value={name}
                placeholder="default"
                disabled={blocked}
                onChange={(e) => setName(e.target.value)}
              />
            </label>
            <div style={{ flex: 'none' }}>
              <button
                className="btn"
                onClick={create}
                disabled={busy || blocked}
                title={blocked ? access?.reason : undefined}
              >
                {busy ? t('keys.create.creating') : t('keys.create.submit')}
              </button>
            </div>
          </div>
          {blocked && (
            <p className="panel-note" style={{ marginBottom: 0 }}>
              <Trans i18nKey="keys.create.blockedNote" components={{ strong: <strong /> }} />
            </p>
          )}
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          {t('keys.list.title')}
          <span className="spacer" />
          {user.is_admin && (
            <label className="check">
              <input
                type="checkbox"
                checked={showAll}
                onChange={(e) => setShowAll(e.target.checked)}
              />
              {t('keys.list.showAll')}
            </label>
          )}
          <button className="btn subtle sm" onClick={() => void load()}>
            {t('common.refresh')}
          </button>
        </div>
        <div className="panel-body" style={{ padding: 0 }}>
          {keys.length === 0 ? (
            <div className="empty">
              {blocked ? t('keys.list.emptyBlocked') : t('keys.list.empty')}
            </div>
          ) : (
            <table>
              {/* The Key column replaces the old Prefix one -- redundant once the key itself is
                  readable -- and is wider, because it carries text plus two buttons. */}
              <colgroup>
                <col style={{ width: '17%' }} />
                <col style={{ width: '28%' }} />
                {showAll && <col style={{ width: '13%' }} />}
                <col style={{ width: '14%' }} />
                <col style={{ width: '14%' }} />
                <col style={{ width: '7%' }} />
                <col style={{ width: '15%' }} />
              </colgroup>
              <thead>
                <tr>
                  <th>{t('keys.table.name')}</th>
                  <th>{t('keys.table.key')}</th>
                  {showAll && <th>{t('keys.table.owner')}</th>}
                  <th>{t('keys.table.created')}</th>
                  <th>{t('keys.table.lastUsed')}</th>
                  <th className="num">{t('keys.table.calls')}</th>
                  <th>{t('common.actions')}</th>
                </tr>
              </thead>
              <tbody>
                {keys.map((k) => (
                  <tr key={k.id} style={{ cursor: 'default' }}>
                    <td className="truncate">
                      {k.name}
                      {k.disabled && (
                        <span className="badge error" style={{ marginLeft: 6 }}>
                          {t('keys.table.disabled')}
                        </span>
                      )}
                    </td>
                    <td className="nowrap">
                      {k.key ? (
                        <span className="key-cell">
                          <code className="mono truncate">
                            {revealedId === k.id ? k.key : mask(k.prefix)}
                          </code>
                          <button
                            className="btn ghost sm"
                            onClick={() => setRevealedId(revealedId === k.id ? null : k.id)}
                          >
                            {revealedId === k.id ? t('common.hide') : t('common.show')}
                          </button>
                          {/* Copy does not reveal: the common case is pasting into a client
                              config, which needs no eyes on the value. */}
                          <button className="btn ghost sm" onClick={() => copy(k.id, k.key!)}>
                            {copiedId === k.id ? t('common.copied') : t('common.copy')}
                          </button>
                        </span>
                      ) : (
                        // No plaintext to show, for one of two quite different reasons -- the
                        // cross-user view withholds every key by design, whereas in one's own
                        // list it means a key predating the stored plaintext (the hash is
                        // one-way, so that one can only be recreated; it keeps working meanwhile).
                        <span className="key-cell">
                          <code className="mono truncate dim">{k.prefix}…</code>
                          <span
                            className="dim"
                            title={t(showAll ? 'keys.table.keyOwnerOnlyHint' : 'keys.table.keyUnavailableHint')}
                          >
                            {t(showAll ? 'keys.table.keyOwnerOnly' : 'keys.table.keyUnavailable')}
                          </span>
                        </span>
                      )}
                    </td>
                    {showAll && <td className="truncate">{k.user_login}</td>}
                    <td className="mono nowrap">{fmt(k.created_at)}</td>
                    <td className="mono nowrap">{fmt(k.last_used_at)}</td>
                    <td className="num">{k.request_count}</td>
                    <td className="nowrap">
                      <button
                        className="btn ghost sm"
                        onClick={() =>
                          setKeyDisabled(k.id, !k.disabled).then(load).catch((e) => setError(String(e)))
                        }
                      >
                        {k.disabled ? t('keys.table.enable') : t('keys.table.disable')}
                      </button>{' '}
                      <button
                        className="btn danger sm"
                        onClick={() => {
                          if (!confirm(t('keys.table.confirmDelete', { name: k.name }))) return
                          deleteKey(k.id).then(load).catch((e) => setError(String(e)))
                        }}
                      >
                        {t('common.delete')}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </div>
  )
}

/** "Can I create a key?" -- the verdict, the reason, and the evidence line by line.
 *
 *  The evidence is laid out for the user because when the policy denies them, their only
 *  route to access is asking an administrator to add their organization to the allow
 *  list -- and for that they need to know which organizations were checked and what came
 *  back.
 */
function AccessPanel({
  access,
  error,
  showDetail,
  onToggleDetail,
  onRefresh,
}: {
  access: AccessVerdict | null
  error: string
  showDetail: boolean
  onToggleDetail: () => void
  onRefresh: () => void
}) {
  const { t } = useTranslation()
  // Keyed on the backend's machine-readable `kind` enum, so it maps straight to catalog keys
  const kindLabel = (kind: string) => t(`access.kind.${kind}`, { defaultValue: kind })

  if (error) {
    return (
      <div className="panel">
        <div className="panel-head">
          {t('keys.access.title')}
          <span className="badge warn">{t('keys.access.unknown')}</span>
          <span className="spacer" />
          <button className="btn subtle sm" onClick={onRefresh}>{t('common.retry')}</button>
        </div>
        <div className="panel-body">
          <p className="panel-note" style={{ margin: 0 }}>{error}</p>
        </div>
      </div>
    )
  }
  if (!access) {
    return (
      <div className="panel">
        <div className="panel-head">{t('keys.access.title')}</div>
        <div className="panel-body">
          <div className="empty">{t('keys.access.checking')}</div>
        </div>
      </div>
    )
  }

  const ok = access.allowed
  const matched = access.matched

  return (
    <div className="panel">
      <div className="panel-head">
        {t('keys.access.title')}
        {ok ? (
          <span className="badge ok">{t('keys.access.allowed')}</span>
        ) : (
          <span className="badge error">{t('keys.access.denied')}</span>
        )}
        {access.policy_enabled && <span className="badge">{t('keys.access.policyOn')}</span>}
        <span className="spacer" />
        {access.detail.length > 0 && (
          <button className="btn ghost sm" onClick={onToggleDetail}>
            {showDetail ? t('keys.access.hideEvidence') : t('keys.access.showEvidence')}
          </button>
        )}
        <button className="btn subtle sm" onClick={onRefresh}>{t('keys.access.recheck')}</button>
      </div>
      <div className="panel-body">
        <p className="panel-note" style={{ marginTop: 0, marginBottom: matched || !ok ? 10 : 0 }}>
          {access.reason}
        </p>

        {matched && (
          <dl className="kv">
            <dt>{t('keys.access.account')}</dt>
            <dd className="mono">{access.login}</dd>
            <dt>{t('keys.access.grantedVia')}</dt>
            <dd>
              {kindLabel(matched.kind)}
              {matched.name && <> · <span className="mono">{matched.name}</span></>}
              {/* The numeric id alongside the team name: the user quotes the name to an
                  administrator, who verifies it on GitHub by id */}
              {matched.kind === 'team' && matched.id && matched.id !== matched.name && (
                <span className="dim mono"> #{matched.id}</span>
              )}
              {matched.enterprise && (
                <>
                  {' '}
                  <span className="dim">
                    {t('keys.access.enterpriseParenOpen')}
                  </span>
                  <span className="mono">{matched.enterprise}</span>
                  <span className="dim">{t('keys.access.enterpriseParenClose')}</span>
                </>
              )}
            </dd>
          </dl>
        )}

        {!ok && (
          <p className="panel-note" style={{ marginBottom: 0 }}>
            <Trans
              i18nKey="keys.access.deniedExplanation"
              components={{ strong: <strong />, code: <code /> }}
            />
          </p>
        )}

        {showDetail && access.detail.length > 0 && (
          <table style={{ marginTop: 12 }}>
            <colgroup>
              <col style={{ width: '30%' }} />
              <col style={{ width: '20%' }} />
              <col style={{ width: '32%' }} />
              <col style={{ width: '18%' }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t('keys.access.table.enterprise')}</th>
                <th>{t('keys.access.table.scope')}</th>
                <th>{t('keys.access.table.name')}</th>
                <th>{t('keys.access.table.isMember')}</th>
                <th>{t('keys.access.table.source')}</th>
              </tr>
            </thead>
            <tbody>
              {access.detail.map((d, i) => (
                <tr key={`${d.enterprise}-${d.kind}-${d.name}-${i}`} style={{ cursor: 'default' }}>
                  <td className="mono truncate">{d.enterprise}</td>
                  <td>{kindLabel(d.kind)}</td>
                  <td className="mono truncate">
                    {d.name}
                    {/* Team: the name leads, the numeric id is secondary -- administrators
                        cross-check GitHub by id */}
                    {d.kind === 'team' && d.id && d.id !== d.name && (
                      <span className="dim"> #{d.id}</span>
                    )}
                    {d.kind === 'org-scan' && d.scanned !== undefined && (
                      <span className="dim">
                        {d.truncated
                          ? t('keys.access.scannedTruncated', { count: d.scanned })
                          : t('keys.access.scanned', { count: d.scanned })}
                      </span>
                    )}
                  </td>
                  <td>
                    {d.member === true ? (
                      <span className="badge ok">{t('common.yes')}</span>
                    ) : d.member === false ? (
                      <span className="dim">{t('common.no')}</span>
                    ) : (
                      <span className="badge warn" title={t('keys.access.undecidableHint')}>
                        {t('keys.access.undecidable')}
                      </span>
                    )}
                  </td>
                  <td>
                    {/* Where the answer came from. A decision whose provenance is invisible is a
                        decision nobody can debug -- 'live' rows are the ones still costing a
                        GitHub round trip per check. */}
                    {d.source && (
                      <span
                        className={`badge ${d.source === 'live' ? 'warn' : ''}`}
                        title={t(`keys.access.source.${d.source}Hint`)}
                      >
                        {t(`keys.access.source.${d.source}`)}
                      </span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}
