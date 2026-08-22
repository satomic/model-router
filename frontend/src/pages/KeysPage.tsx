import { Fragment, useCallback, useEffect, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import {
  createKey,
  deleteKey,
  getAvailableModels,
  getKeys,
  getMyAccess,
  patchKey,
  setKeyDisabled,
  type AccessVerdict,
  type ApiKey,
  type KeyScope,
  type SessionUser,
} from '../api'
import { copyText } from '../clipboard'
import CliExamples from '../components/CliExamples'
import { useDialogs } from '../components/Dialog'
import ScopeEditor, {
  scopeIsComplete,
  sameScope,
  type ScopeModel,
} from '../components/ScopeEditor'
import { formatDateTime } from '../i18n/format'
import { keyPolicyReason, keyScopeReason } from '../reasons'

/** Unix seconds -> a locale-formatted timestamp, or an em dash when never used. */
const fmt = (ts: number | null) => (ts ? formatDateTime(ts * 1000) : '—')

/** How much of a key stays visible while it is masked -- the same 12 characters the backend
 *  puts in `prefix`, so masked and unavailable rows line up. */
const mask = (prefix: string) => `${prefix}${'•'.repeat(12)}`

/**
 * One clipboard helper for the whole page.
 *
 * `copiedId` rather than a boolean, so the confirmation appears on the row that was actually
 * copied. The Clipboard API is absent on a non-secure origin (plain http on anything but
 * localhost), which is a realistic way to run this console, so the actual copying goes through
 * src/clipboard.ts and its textarea fallback, and a genuine refusal is surfaced instead of
 * leaving a button that does nothing.
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
    // Through the shared helper rather than navigator.clipboard directly: that object does not
    // exist at all on a non-secure origin, so reading .writeText off it threw inside this
    // handler and the button did nothing, silently, with no error to show.
    void copyText(text).then((ok) =>
      ok ? setCopiedId(id) : setCopyError(t('keys.copyFailed')),
    )
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
  const dialogs = useDialogs()
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
  /** The scope the create form will apply. Defaults to unrestricted, which is what a key
   *  created before scopes existed also means. */
  const [scope, setScope] = useState<KeyScope>({ kind: 'all' })
  /** The caller's own model list, for the "specific models" kind. Only the owner's models are
   *  offered: a scope can never widen, so anything else would be a field that cannot work. */
  const [myModels, setMyModels] = useState<ScopeModel[]>([])
  /** Which row is being edited, and the draft for it. One at a time, so an edit cannot be
   *  half-applied across two rows. */
  const [editingId, setEditingId] = useState<string | null>(null)
  const [draft, setDraft] = useState<KeyScope>({ kind: 'all' })
  const [savingScope, setSavingScope] = useState(false)
  /** Which row has its command-line examples open. Independent of `editingId`: reading the
   *  snippet while narrowing the scope is a reasonable thing to be doing. */
  const [examplesId, setExamplesId] = useState<string | null>(null)

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

  useEffect(() => {
    // A failure here is deliberately not surfaced: the model list only feeds the optional
    // "specific models" kind, and the two other kinds stay usable without it.
    getAvailableModels()
      .then((r) =>
        setMyModels(
          r.models.map((m) => ({ name: m, api_type: r.catalog[m]?.api_type ?? '' })),
        ),
      )
      .catch(() => setMyModels([]))
  }, [])

  const create = async () => {
    setBusy(true)
    setError('')
    try {
      // Deliberately not `scope`: with the editor hidden the draft cannot have been changed,
      // and sending the default explicitly means a verdict that arrived late cannot produce a
      // request that 403s.
      const created = await createKey(name.trim() || 'default', scopeLocked ? { kind: 'all' } : scope)
      setFresh(created)
      setName('')
      setScope({ kind: 'all' })
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

  const saveScope = async (id: string) => {
    setSavingScope(true)
    setError('')
    try {
      await patchKey(id, { scope: draft })
      setEditingId(null)
      await load()
    } catch (e) {
      setError(String(e))
    } finally {
      setSavingScope(false)
    }
  }

  /** The button is not disabled while the verdict is still loading: the backend enforces
   *  the rule anyway, so one slow request should not lock up the UI. */
  const blocked = access !== null && !access.allowed
  /** Whether this account may narrow a key at all. Same "not while loading" posture as above,
   *  and the same division of labour: the console hides what is not permitted, the backend is
   *  what actually refuses it (POST/PATCH /v1/keys answer 403). */
  const scopeLocked = access !== null && !access.key_scope.allowed

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
              <CliExamples
                keyValue={fresh.key}
                login={fresh.user_login}
                copy={copy}
                copiedId={copiedId}
                copyId="cli-fresh"
              />
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
                disabled={busy || blocked || !scopeIsComplete(scope)}
                title={blocked ? keyPolicyReason(t, access) : undefined}
              >
                {busy ? t('keys.create.creating') : t('keys.create.submit')}
              </button>
            </div>
          </div>

          <div className="field" style={{ marginTop: 14, marginBottom: 0 }}>
            <span className="field-name">
              {t('keys.scope.title')}
              <span className="field-hint">{t('keys.scope.hint')}</span>
            </span>
            {scopeLocked ? (
              /* The server's own verdict, not a generic sentence: it names which level the
                 account failed to match, and asking an administrator is the only route to the
                 capability, so the user needs to know what to ask to be added to. Rendered from
                 the verdict's reason code, because the backend says it in English only. */
              <p className="panel-note" style={{ margin: 0 }}>
                <span className="badge warn">{t('keys.scope.lockedBadge')}</span>{' '}
                {keyScopeReason(t, access?.key_scope ?? null)}
              </p>
            ) : (
              <ScopeEditor value={scope} onChange={setScope} models={myModels} disabled={blocked} />
            )}
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
                <col style={{ width: '12%' }} />
                <col style={{ width: '22%' }} />
                {showAll && <col style={{ width: '10%' }} />}
                <col style={{ width: '10%' }} />
                {/* Both timestamps render as a full local date and time, so 14% is the width at
                    which they stop being cut off mid-value. */}
                <col style={{ width: '14%' }} />
                <col style={{ width: '14%' }} />
                <col style={{ width: '5%' }} />
                <col style={{ width: '23%' }} />
              </colgroup>
              <thead>
                <tr>
                  <th>{t('keys.table.name')}</th>
                  <th>{t('keys.table.key')}</th>
                  {showAll && <th>{t('keys.table.owner')}</th>}
                  <th>{t('keys.table.scope')}</th>
                  <th>{t('keys.table.created')}</th>
                  <th>{t('keys.table.lastUsed')}</th>
                  <th className="num">{t('keys.table.calls')}</th>
                  <th>{t('common.actions')}</th>
                </tr>
              </thead>
              <tbody>
                {keys.map((k) => (
                  <Fragment key={k.id}>
                  <tr style={{ cursor: 'default' }}>
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
                    <td>
                      <ScopeSummary scope={k.scope} />
                    </td>
                    <td className="mono nowrap">{fmt(k.created_at)}</td>
                    <td className="mono nowrap">{fmt(k.last_used_at)}</td>
                    <td className="num">{k.request_count}</td>
                    <td className="nowrap">
                      {/* The scope is editable after the fact, on purpose: a key handed to a CI
                          job is usually narrowed once its needs are known, not at the moment it
                          is created. */}
                      {/* Ahead of the scope button because it is the one a user reaches for
                          repeatedly: the snippet is what turns a key in a table into a working
                          client, and it was previously shown once and then lost. */}
                      <button
                        className="btn ghost sm"
                        onClick={() => {
                          // The snippet spells the key out, so the row it belongs to is unmasked
                          // with it rather than left contradicting it -- and, since both states
                          // hold one id, exactly one key is on screen either way.
                          const open = examplesId === k.id
                          setExamplesId(open ? null : k.id)
                          setRevealedId(open ? null : k.id)
                        }}
                      >
                        {examplesId === k.id ? t('keys.cli.close') : t('keys.cli.show')}
                      </button>{' '}
                      {/* Hidden where there is nothing the button could do: an account that may
                          not narrow a key has no edit to make on an unrestricted one. A key that
                          already carries a scope keeps its button, because widening back to
                          "everything" stays allowed and a key stuck narrow would be worse. */}
                      {(!scopeLocked || (k.scope && k.scope.kind !== 'all')) && (
                        <>
                          <button
                            className="btn ghost sm"
                            onClick={() => {
                              if (editingId === k.id) return setEditingId(null)
                              setDraft(k.scope ?? { kind: 'all' })
                              setEditingId(k.id)
                            }}
                          >
                            {editingId === k.id ? t('keys.scope.close') : t('keys.scope.edit')}
                          </button>{' '}
                        </>
                      )}
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
                        onClick={async () => {
                          const yes = await dialogs.confirm({
                            title: t('keys.table.confirmDeleteTitle'),
                            message: t('keys.table.confirmDelete', { name: k.name }),
                            confirmLabel: t('common.delete'),
                            danger: true,
                          })
                          if (!yes) return
                          deleteKey(k.id).then(load).catch((e) => setError(String(e)))
                        }}
                      >
                        {t('common.delete')}
                      </button>
                    </td>
                  </tr>
                  {examplesId === k.id && (
                    <tr style={{ cursor: 'default' }}>
                      <td colSpan={showAll ? 8 : 7} className="scope-row">
                        <div className="field-name" style={{ marginBottom: 10 }}>
                          {t('keys.cli.rowTitle', { name: k.name })}
                        </div>
                        <CliExamples
                          keyValue={k.key ?? null}
                          login={k.user_login}
                          // Two quite different reasons for a missing value, and the fix differs:
                          // another user's key is never shown here at all, whereas one's own key
                          // can predate the stored plaintext and has to be recreated to be read.
                          missing={showAll && k.user_login !== user.login ? 'otherUser' : 'unavailable'}
                          copy={copy}
                          copiedId={copiedId}
                          copyId={`cli-${k.id}`}
                        />
                      </td>
                    </tr>
                  )}
                  {editingId === k.id && (
                    <tr style={{ cursor: 'default' }}>
                      <td colSpan={showAll ? 8 : 7} className="scope-row">
                        <div className="field-name">
                          {t('keys.scope.editTitle', { name: k.name })}
                          <span className="field-hint">{t('keys.scope.hint')}</span>
                        </div>
                        {scopeLocked && (
                          <p className="panel-note" style={{ marginTop: 0 }}>
                            <span className="badge warn">{t('keys.scope.lockedBadge')}</span>{' '}
                            {t('keys.scope.widenOnly')}
                          </p>
                        )}
                        <ScopeEditor
                          value={draft}
                          onChange={setDraft}
                          models={myModels}
                          disabled={savingScope}
                          // Clearing the restriction is the only edit the backend will accept from
                          // an account that may not narrow a key, so it is the only one offered.
                          widenOnly={scopeLocked}
                        />
                        <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
                          <button
                            className="btn sm"
                            disabled={
                              savingScope || !scopeIsComplete(draft) || sameScope(draft, k.scope)
                            }
                            onClick={() => void saveScope(k.id)}
                          >
                            {savingScope ? t('common.saving') : t('keys.scope.save')}
                          </button>
                          <button
                            className="btn subtle sm"
                            disabled={savingScope}
                            onClick={() => setEditingId(null)}
                          >
                            {t('keys.scope.cancel')}
                          </button>
                        </div>
                      </td>
                    </tr>
                  )}
                  </Fragment>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </div>
  )
}

/** The one-line form of a key's scope, for the list.
 *
 *  Unrestricted is stated rather than left blank: an empty cell reads as missing data, and
 *  "this key can reach everything you can" is the single most important thing to be able to
 *  see at a glance.
 */
function ScopeSummary({ scope }: { scope?: KeyScope }) {
  const { t } = useTranslation()
  const kind = scope?.kind ?? 'all'
  if (kind === 'all') return <span className="dim">{t('keys.scope.summary.all')}</span>
  if (scope && scope.kind === 'api_types') {
    return (
      <span className="scope-summary">
        {scope.api_types.map((tp) => (
          <span className="badge" key={tp}>{tp}</span>
        ))}
      </span>
    )
  }
  const models = scope && scope.kind === 'models' ? scope.models : []
  return (
    <span className="scope-summary" title={models.join(', ')}>
      <span className="badge">{t('keys.scope.summary.models', { count: models.length })}</span>
    </span>
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
          {keyPolicyReason(t, access)}
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
