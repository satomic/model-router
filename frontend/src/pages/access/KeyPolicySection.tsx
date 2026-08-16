import { useCallback, useEffect, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import {
  discoverEnterprises,
  getCacheStatus,
  getTokenStatus,
  refreshCache,
  verifyToken,
  type CacheStatus,
  type DiscoveredEnterprise,
  type EnterpriseRule,
  type TokenOwner,
  type TokenStatus,
} from '../../api'
import { formatDateTime } from '../../i18n/format'
import type { AccessSectionProps } from './types'

/** The enterprise key policy: a per-Enterprise master switch plus a second level of
 *  organization / enterprise-team rules.
 *
 *  Why the gate sits at "create a key" rather than at "sign in": once GitHub OAuth is
 *  configured any account can sign in, but without an API key it cannot call
 *  /v1/chat/completions and cannot use BYOK. So the authorization decision happens at key
 *  creation, driven by live GitHub membership.
 */
export default function KeyPolicySection({ auth, set, saved, notify }: AccessSectionProps) {
  const { t } = useTranslation()
  const policy = auth.key_policy ?? {}
  const savedPolicy = saved.key_policy ?? {}
  const enterprises = policy.enterprises ?? {}

  const [tokenStatus, setTokenStatus] = useState<TokenStatus | null>(null)
  const [discovered, setDiscovered] = useState<DiscoveredEnterprise[] | null>(null)
  const [loading, setLoading] = useState(false)
  const [checking, setChecking] = useState(false)
  const [draftOwner, setDraftOwner] = useState<TokenOwner | null>(null)
  const [reveal, setReveal] = useState(false)
  const [discoverError, setDiscoverError] = useState('')

  /** Whether the token is persisted: only a saved token can be used by the backend to fetch
   *  the enterprise list. */
  const tokenSaved = Boolean((savedPolicy.github_token ?? '').trim())
  const tokenDraftChanged = (policy.github_token ?? '') !== (savedPolicy.github_token ?? '')

  const setPolicy = (patch: Partial<typeof policy>) =>
    set({ key_policy: { ...policy, ...patch } })

  const setRule = (slug: string, patch: Partial<EnterpriseRule>) =>
    setPolicy({
      enterprises: { ...enterprises, [slug]: { ...(enterprises[slug] ?? {}), ...patch } },
    })

  const loadToken = useCallback(() => {
    getTokenStatus()
      .then(setTokenStatus)
      .catch(() => setTokenStatus(null))
  }, [])

  useEffect(() => {
    loadToken()
  }, [loadToken])

  const discover = async (refresh = false) => {
    setLoading(true)
    setDiscoverError('')
    try {
      const { enterprises: list } = await discoverEnterprises(refresh)
      setDiscovered(list)
      if (!list.length) {
        setDiscoverError(t('access.policy.noEnterprisesVisible'))
      }
    } catch (e) {
      setDiscoverError(String(e))
    } finally {
      setLoading(false)
    }
  }

  // Fetch once automatically when the token is already saved, saving one click
  useEffect(() => {
    if (tokenSaved && discovered === null && !loading) void discover()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tokenSaved])

  const check = async () => {
    setChecking(true)
    try {
      const owner = await verifyToken(policy.github_token ?? '')
      setDraftOwner(owner)
      notify(
        'ok',
        owner.scopes.length
          ? t('access.policy.tokenValidWithScopes', {
              login: owner.login,
              scopes: owner.scopes.join(', '),
            })
          : t('access.policy.tokenValid', { login: owner.login }),
      )
    } catch (e) {
      setDraftOwner(null)
      notify('error', String(e))
    } finally {
      setChecking(false)
    }
  }

  const enabled = Boolean(policy.enabled)
  const activeCount = Object.values(enterprises).filter((r) => r?.enabled).length

  return (
    <>
      {/* -- Master switch -- */}
      <div className="panel">
        <div className="panel-head">
          {t('access.policy.title')}
          {enabled ? (
            <span className="badge ok">{t('access.policy.enabled')}</span>
          ) : (
            <span className="badge warn">{t('access.policy.disabled')}</span>
          )}
        </div>
        <div className="panel-body">
          <label className="check">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => setPolicy({ enabled: e.target.checked })}
            />
            {t('access.policy.toggle')}
          </label>
          <p className="panel-note" style={{ marginTop: 10, marginBottom: 0 }}>
            {enabled ? (
              t('access.policy.enabledNote')
            ) : (
              <Trans i18nKey="access.policy.disabledNote" components={{ strong: <strong /> }} />
            )}
          </p>
          {enabled && !tokenSaved && (
            <p className="panel-note" style={{ marginTop: 10, marginBottom: 0 }}>
              <span className="badge error">{t('access.policy.missingTokenBadge')}</span>{' '}
              <Trans i18nKey="access.policy.missingTokenNote" components={{ strong: <strong /> }} />
            </p>
          )}
        </div>
      </div>

      {/* -- Enterprise administrator token -- */}
      <div className="panel">
        <div className="panel-head">
          {t('access.policy.tokenTitle')}
          {tokenStatus?.configured && !tokenStatus.error && (
            <span className="badge ok">{t('access.policy.tokenOk')}</span>
          )}
          {tokenStatus?.error && <span className="badge error">{t('access.policy.tokenBad')}</span>}
        </div>
        <div className="panel-body">
          <label className="field">
            <span className="field-name">
              Personal Access Token
              <span className="field-hint">{t('access.policy.tokenHint')}</span>
            </span>
            <div className="secret-reveal">
              <input
                type={reveal ? 'text' : 'password'}
                className="mono"
                value={policy.github_token ?? ''}
                placeholder={
                  tokenSaved
                    ? t('access.policy.tokenSavedPlaceholder', { hint: tokenStatus?.hint ?? '' })
                    : 'ghp_...'
                }
                onChange={(e) => setPolicy({ github_token: e.target.value.trim() })}
              />
              <button className="btn ghost sm" onClick={() => setReveal((v) => !v)}>
                {reveal ? t('common.hide') : t('common.show')}
              </button>
              <button className="btn subtle sm" onClick={check} disabled={checking}>
                {checking ? t('access.policy.verifying') : t('access.policy.verify')}
              </button>
            </div>
          </label>

          {(draftOwner || tokenStatus?.owner) && (
            <dl className="kv">
              <dt>{t('access.policy.tokenOwner')}</dt>
              <dd className="mono">{(draftOwner ?? tokenStatus?.owner)!.login}</dd>
              <dt>scope</dt>
              <dd className="mono">
                {(draftOwner ?? tokenStatus?.owner)!.scopes.join(', ') ||
                  t('access.policy.fineGrainedToken')}
              </dd>
              <dt>{t('access.policy.canListEnterprises')}</dt>
              <dd>
                {(draftOwner ?? tokenStatus?.owner)!.has_enterprise_scope ? (
                  <span className="badge ok">{t('common.yes')}</span>
                ) : (
                  <span className="badge error">{t('access.policy.missingEnterpriseScope')}</span>
                )}
              </dd>
            </dl>
          )}
          {tokenStatus?.error && (
            <p className="panel-note" style={{ marginBottom: 0 }}>
              <span className="badge error">{t('access.policy.githubError')}</span>{' '}
              {tokenStatus.error}
            </p>
          )}
          {tokenDraftChanged && (
            <p className="panel-note" style={{ marginBottom: 0 }}>
              <Trans i18nKey="access.policy.tokenUnsaved" components={{ strong: <strong /> }} />
            </p>
          )}
        </div>
      </div>

      {/* -- The local copy of the structure and the member lists -- */}
      <CacheCard
        refreshSeconds={policy.cache_refresh_seconds}
        onChangeInterval={(v) => setPolicy({ cache_refresh_seconds: v })}
        tokenSaved={tokenSaved}
        notify={notify}
      />

      {/* -- The enterprise structure fetched from GitHub -- */}
      <div className="panel">
        <div className="panel-head">
          {t('access.policy.structureTitle')}
          {discovered && (
            <span className="badge">
              {t('access.policy.enterpriseCount', { count: discovered.length })}
            </span>
          )}
          <span className="spacer" />
          <button
            className="btn subtle sm"
            onClick={() => void discover(true)}
            disabled={loading || !tokenSaved}
          >
            {loading ? t('access.policy.fetching') : t('access.policy.refetch')}
          </button>
        </div>
        <div className="panel-body" style={{ paddingBottom: discovered?.length ? 4 : undefined }}>
          {!tokenSaved ? (
            <div className="empty">{t('access.policy.needTokenFirst')}</div>
          ) : discoverError ? (
            <p className="panel-note" style={{ margin: 0 }}>
              <span className="badge error">{t('access.policy.fetchFailed')}</span> {discoverError}
            </p>
          ) : loading && !discovered ? (
            <div className="empty">{t('access.policy.fetchingFromGitHub')}</div>
          ) : !discovered?.length ? (
            <div className="empty">{t('access.policy.noEnterprises')}</div>
          ) : (
            <>
              <p className="panel-note" style={{ marginTop: 0 }}>
                <Trans i18nKey="access.policy.structureLead" components={{ strong: <strong /> }} />
              </p>
              {discovered.map((ent) => (
                <EnterpriseCard
                  key={ent.slug}
                  ent={ent}
                  rule={enterprises[ent.slug] ?? {}}
                  onChange={(patch) => setRule(ent.slug, patch)}
                />
              ))}
            </>
          )}
        </div>
      </div>

      {enabled && tokenSaved && activeCount === 0 && (
        <div className="toast warn">
          <Trans i18nKey="access.policy.nobodyAllowed" components={{ strong: <strong /> }} />
        </div>
      )}
    </>
  )
}

/** The state of data/github/: how old the local copy is, which scopes it covers, and where it is
 *  incomplete.
 *
 *  Incompleteness is the point of this card. A truncated or errored member list is never treated as
 *  authoritative -- those scopes fall back to a live GitHub call per check -- so an administrator
 *  wondering why the console is still slow needs to be able to see which scopes are in that state.
 *  Counts only, never logins: an organization's membership is not something a status panel
 *  should publish.
 */
function CacheCard({
  refreshSeconds,
  onChangeInterval,
  tokenSaved,
  notify,
}: {
  refreshSeconds?: number
  onChangeInterval: (value: number) => void
  tokenSaved: boolean
  notify: AccessSectionProps['notify']
}) {
  const { t } = useTranslation()
  const [status, setStatus] = useState<CacheStatus | null>(null)
  const [busy, setBusy] = useState(false)

  const load = useCallback(() => {
    getCacheStatus().then(setStatus).catch(() => setStatus(null))
  }, [])

  useEffect(() => {
    load()
  }, [load])

  async function forceRefresh() {
    setBusy(true)
    try {
      setStatus(await refreshCache())
      notify('ok', t('access.cache.refreshed'))
    } catch (e) {
      notify('error', String(e))
    } finally {
      setBusy(false)
    }
  }

  const scopes = status?.members.scopes ?? []
  const problems = (status?.members.truncated_scopes ?? 0) + (status?.members.errored_scopes ?? 0)

  return (
    <div className="panel">
      <div className="panel-head">
        {t('access.cache.title')}
        {status && (status.stale ? (
          <span className="badge warn">{t('access.cache.stale')}</span>
        ) : (
          <span className="badge ok">{t('access.cache.fresh')}</span>
        ))}
        {status && status.token_matches === false && (
          <span className="badge error">{t('access.cache.tokenChanged')}</span>
        )}
        <span className="spacer" />
        <button className="btn subtle sm" onClick={forceRefresh} disabled={busy || !tokenSaved}>
          {busy ? t('access.cache.refreshing') : t('access.cache.refreshNow')}
        </button>
      </div>
      <div className="panel-body">
        <p className="panel-note" style={{ marginTop: 0 }}>
          <Trans i18nKey="access.cache.lead" components={{ strong: <strong />, code: <code /> }} />
        </p>

        <label className="field">
          <span className="field-name">
            {t('access.cache.interval')}
            <span className="field-hint">{t('access.cache.intervalHint')}</span>
          </span>
          <input
            type="number"
            min={60}
            className="mono"
            value={refreshSeconds ?? 3600}
            onChange={(e) => onChangeInterval(Math.max(60, Number(e.target.value) || 3600))}
          />
        </label>

        {!status ? (
          <div className="empty">{t('access.cache.unavailable')}</div>
        ) : (
          <>
            <dl className="kv">
              <dt>{t('access.cache.structureFetched')}</dt>
              <dd>
                {status.structure.fetched_at
                  ? formatDateTime(status.structure.fetched_at * 1000)
                  : t('access.cache.never')}
              </dd>
              <dt>{t('access.cache.membersFetched')}</dt>
              <dd>
                {status.members.fetched_at
                  ? formatDateTime(status.members.fetched_at * 1000)
                  : t('access.cache.never')}
              </dd>
              <dt>{t('access.cache.scopeCount')}</dt>
              <dd>
                {t('access.cache.scopes', { count: scopes.length })}
                {problems > 0 && (
                  <>
                    {' '}
                    <span className="badge warn">
                      {t('access.cache.incomplete', { count: problems })}
                    </span>
                  </>
                )}
              </dd>
              <dt>{t('access.cache.probeCount')}</dt>
              <dd>{t('access.cache.probes', { count: status.probes.count })}</dd>
            </dl>

            {status.error && (
              <p className="panel-note">
                <span className="badge error">{t('common.error')}</span> {status.error}
              </p>
            )}

            {scopes.length > 0 && (
              <table>
                <thead>
                  <tr>
                    <th>{t('access.cache.table.scope')}</th>
                    <th>{t('access.cache.table.name')}</th>
                    <th>{t('access.cache.table.members')}</th>
                    <th>{t('access.cache.table.state')}</th>
                  </tr>
                </thead>
                <tbody>
                  {scopes.map((s) => (
                    <tr key={s.key}>
                      <td>{t(`access.kind.${s.kind}`, { defaultValue: s.kind })}</td>
                      <td className="mono truncate">{s.name}</td>
                      <td className="mono">{s.count}</td>
                      <td>
                        {s.error ? (
                          <span className="badge error" title={s.error}>
                            {t('access.cache.table.errored')}
                          </span>
                        ) : s.truncated ? (
                          <span className="badge warn">{t('access.cache.table.truncated')}</span>
                        ) : (
                          <span className="badge ok">{t('access.cache.table.complete')}</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </>
        )}
      </div>
    </div>
  )
}

/** A single enterprise: master switch plus organization / enterprise-team rules. */
function EnterpriseCard({
  ent,
  rule,
  onChange,
}: {
  ent: DiscoveredEnterprise
  rule: EnterpriseRule
  onChange: (patch: Partial<EnterpriseRule>) => void
}) {
  const { t } = useTranslation()
  const on = Boolean(rule.enabled)
  const allOrgs = Boolean(rule.allow_all_orgs)
  const orgs = (rule.organizations ?? []).map(String)
  const teams = (rule.teams ?? []).map(String)

  const toggleOrg = (login: string, checked: boolean) =>
    onChange({
      organizations: checked ? [...orgs, login] : orgs.filter((o) => o !== login),
    })

  const toggleTeam = (id: number, checked: boolean) =>
    onChange({
      teams: checked
        ? [...teams, String(id)]
        : teams.filter((teamId) => teamId !== String(id)),
    })

  return (
    <div className="provider-card">
      <div className="panel-head" style={{ borderBottom: 'none', paddingLeft: 0, paddingRight: 0 }}>
        <label className="check" style={{ fontWeight: 600 }}>
          <input
            type="checkbox"
            checked={on}
            onChange={(e) => onChange({ enabled: e.target.checked })}
          />
          {ent.name}
        </label>
        <span className="badge mono">{ent.slug}</span>
        <span className="spacer" />
        {on ? (
          <span className="badge ok">{t('access.policy.entOn')}</span>
        ) : (
          <span className="dim">{t('access.policy.entOff')}</span>
        )}
      </div>

      {!on ? (
        <p className="panel-note" style={{ margin: 0 }}>
          <Trans i18nKey="access.policy.entOffNote" components={{ strong: <strong /> }} />
        </p>
      ) : (
        <>
          <label className="check" style={{ marginBottom: 8 }}>
            <input
              type="checkbox"
              checked={allOrgs}
              onChange={(e) => onChange({ allow_all_orgs: e.target.checked })}
            />
            <Trans i18nKey="access.policy.allowAllOrgs" components={{ strong: <strong /> }} />
          </label>
          <p className="panel-note" style={{ marginTop: 0 }}>
            {allOrgs ? (
              <>
                {t('access.policy.allOrgsNote')}
                {ent.organizations_truncated && (
                  <>
                    {' '}
                    {/* Both counts sit inside the bolded clause, so the whole sentence is one
                        key with the numbers interpolated */}
                    <Trans
                      i18nKey="access.policy.allOrgsTruncated"
                      values={{
                        total: ent.organizations_total,
                        listed: ent.organizations.length,
                      }}
                      components={{ strong: <strong /> }}
                    />
                  </>
                )}
              </>
            ) : (
              t('access.policy.allOrgsOffNote')
            )}
          </p>

          {/* Organization-level rule */}
          <div className="field-name" style={{ marginBottom: 6 }}>
            {t('access.policy.allowedOrgs')}
            <span className="field-hint">{t('access.policy.allowedOrgsHint')}</span>
          </div>
          {ent.organizations_error ? (
            <p className="panel-note" style={{ marginTop: 0 }}>
              <span className="badge error">{t('access.policy.orgListUnavailable')}</span>{' '}
              {ent.organizations_error}
            </p>
          ) : !ent.organizations.length ? (
            <p className="panel-note" style={{ marginTop: 0 }}>{t('access.policy.noOrgs')}</p>
          ) : (
            <div className="tiles">
              {ent.organizations.map((o) => (
                <label key={o.login} className="tile check" title={o.name}>
                  <input
                    type="checkbox"
                    checked={orgs.includes(o.login)}
                    disabled={allOrgs}
                    onChange={(e) => toggleOrg(o.login, e.target.checked)}
                  />
                  <span className="mono truncate">{o.login}</span>
                </label>
              ))}
            </div>
          )}
          {ent.organizations_truncated && !ent.organizations_error && (
            <p className="panel-note" style={{ marginBottom: 0 }}>
              {t('access.policy.orgsListedPartially', {
                total: ent.organizations_total,
                listed: ent.organizations.length,
              })}
            </p>
          )}

          {/* Enterprise-team-level rule */}
          <div className="field-name" style={{ margin: '14px 0 6px' }}>
            {t('access.policy.allowedTeams')}
            <span className="field-hint">{t('access.policy.allowedTeamsHint')}</span>
          </div>
          {ent.teams_error ? (
            <p className="panel-note" style={{ margin: 0 }}>
              <span className="badge warn">{t('access.policy.teamListUnavailable')}</span>{' '}
              {ent.teams_error}
              {' '}
              {t('access.policy.teamListUnavailableNote')}
            </p>
          ) : !ent.teams.length ? (
            <p className="panel-note" style={{ margin: 0 }}>{t('access.policy.noTeams')}</p>
          ) : (
            <div className="tiles">
              {/* The loop variable is `team`, not `t` -- `t` is the translator in this scope */}
              {ent.teams.map((team) => (
                <label key={team.id} className="tile check" title={team.slug}>
                  <input
                    type="checkbox"
                    checked={teams.includes(String(team.id))}
                    onChange={(e) => toggleTeam(team.id, e.target.checked)}
                  />
                  <span className="truncate">{team.name}</span>
                </label>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  )
}
