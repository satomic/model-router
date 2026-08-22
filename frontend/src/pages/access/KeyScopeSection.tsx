import { useCallback, useEffect, useMemo, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import {
  discoverEnterprises,
  getSignedInUsers,
  type DiscoveredEnterprise,
  type KeyPolicy,
  type KeyScopePolicy,
  type KnownUser,
} from '../../api'
import { useDialogs } from '../../components/Dialog'
import { scopeCandidates, type KeyPolicyReason } from '../../keyeligibility'
import type { AccessSectionProps } from './types'

/** Rows rendered before the list has to be asked for in full. Same cap and same reason as the
 *  model-policy bindings tables: an enterprise here owns 998 organizations, and everything an
 *  administrator came for is sorted above the cap. */
const VISIBLE = 20

/** One pickable entry: a signed-in user, an enterprise team, or an organization. */
interface Candidate {
  /** The value stored in config: a login, an organization login, or '<slug>/<team id>'. */
  key: string
  /** What a human reads: the login, or the team's display name. */
  name: string
  /** Which enterprise a team or organization came from, so two "Platform" teams stay apart. */
  enterprise?: string
  /** Whether this account may create an API key at all. `null` means the verdict is unknown. */
  eligible: boolean | null
  /** Why not, when it may not. A translation key under `access.keyscope.reason.`. */
  reason?: KeyPolicyReason | 'userNotAllowed'
}

/** Who may narrow an API key to specific models or interface types.
 *
 *  This is a cost control rather than a security one. A key scope can only ever subtract from what
 *  its owner is already allowed, so it grants nothing -- but a user who scopes their key to one
 *  expensive model has pinned every request on that key to it, and routing cheap work to a cheap
 *  model stops applying to that key. So the capability is off by default and is granted explicitly.
 *
 *  The three tables offer only accounts that may create an API key in the first place, the same way
 *  the model-policy bindings tables do and for the same reason: the permission to narrow a key is
 *  meaningless to somebody who cannot create one, so listing them here would be configuring
 *  nothing. What is **already listed** stays visible even once it has become ineligible, because a
 *  row that disappears from the page that owns it leaves no way to clear it.
 *
 *  The semantics are stated on the page, not only in the docs, because one part of them is not
 *  guessable from three tables: the levels are combined with AND, but only the ones actually filled
 *  in. A level left empty abstains instead of denying, otherwise listing an organization on its own
 *  would match nobody at all (no login is a member of an empty user list) and the feature would
 *  look broken.
 */
export default function KeyScopeSection({ auth, set, saved }: AccessSectionProps) {
  const { t } = useTranslation()
  const policy = auth.key_scope_policy ?? {}
  const enabled = Boolean(policy.enabled)

  const users = policy.users ?? []
  const teams = policy.teams ?? []
  const orgs = policy.organizations ?? []
  const nothingListed = !users.length && !teams.length && !orgs.length

  // Discovery needs a saved token, so it reads the server's copy rather than the draft.
  const tokenSaved = Boolean((saved.key_policy?.github_token ?? '').trim())
  const structure = useStructure(tokenSaved)
  const knownUsers = useSignedInUsers()

  // The draft, so the tables react to a key-policy edit made in the next tab over without a save
  // in between. The user level cannot follow: its verdict is a membership question only the server
  // can answer, and the server answers it about the saved policy. The users panel says so.
  const keyPolicy: KeyPolicy = auth.key_policy ?? {}
  const gated = Boolean(keyPolicy.enabled)

  const setPolicy = (patch: Partial<KeyScopePolicy>) =>
    set({ key_scope_policy: { ...policy, ...patch } })

  const userCandidates = useMemo<Candidate[] | null>(
    () =>
      knownUsers.data === null
        ? null
        : knownUsers.data.users.map((u) => ({
            key: u.login,
            name: u.login,
            // undefined rather than null when the server was not asked: the request always asks,
            // so an absent field means an older server, and unknown is the honest reading of it.
            eligible: u.can_create_key === undefined ? null : u.can_create_key,
            reason: u.can_create_key === false ? ('userNotAllowed' as const) : undefined,
          })),
    [knownUsers.data],
  )

  const teamCandidates = useMemo<Candidate[] | null>(
    () =>
      structure.discovered === null
        ? null
        : scopeCandidates('teams', structure.discovered, keyPolicy),
    [structure.discovered, keyPolicy],
  )

  const orgCandidates = useMemo<Candidate[] | null>(
    () =>
      structure.discovered === null
        ? null
        : scopeCandidates('organizations', structure.discovered, keyPolicy),
    [structure.discovered, keyPolicy],
  )

  return (
    <>
      {/* -- Master switch and the semantics -- */}
      <div className="panel">
        <div className="panel-head">
          {t('access.keyscope.title')}
          {enabled ? (
            <span className="badge ok">{t('access.keyscope.on')}</span>
          ) : (
            <span className="badge warn">{t('access.keyscope.off')}</span>
          )}
        </div>
        <div className="panel-body">
          <label className="check">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => setPolicy({ enabled: e.target.checked })}
            />
            {t('access.keyscope.toggle')}
          </label>

          <p className="panel-note" style={{ marginTop: 10 }}>
            {enabled ? (
              <Trans i18nKey="access.keyscope.onNote" components={{ strong: <strong /> }} />
            ) : (
              <Trans i18nKey="access.keyscope.offNote" components={{ strong: <strong /> }} />
            )}
          </p>

          <p className="panel-note">
            <Trans i18nKey="access.keyscope.why" components={{ strong: <strong /> }} />
          </p>

          {/* Enabled with nothing listed is the same trap as an enabled key policy with no token:
              it reads as "allow everybody" and means the opposite, so it says so. */}
          {enabled && nothingListed && (
            <p className="panel-note">
              <span className="badge warn">{t('access.keyscope.emptyBadge')}</span>{' '}
              {t('access.keyscope.empty')}
            </p>
          )}

          <ul className="panel-note" style={{ marginBottom: 0 }}>
            <li>
              <Trans i18nKey="access.keyscope.ruleAnd" components={{ strong: <strong /> }} />
            </li>
            <li>
              <Trans i18nKey="access.keyscope.ruleAbstain" components={{ strong: <strong /> }} />
            </li>
            <li>{t('access.keyscope.ruleOr')}</li>
            <li>{t('access.keyscope.ruleAdmin')}</li>
            <li>
              <Trans i18nKey="access.keyscope.ruleExisting" components={{ strong: <strong /> }} />
            </li>
          </ul>
        </div>
      </div>

      <AllowList
        level="users"
        selected={users}
        candidates={userCandidates}
        gated={gated}
        loading={knownUsers.data === null && !knownUsers.error}
        error={knownUsers.error}
        onRefresh={knownUsers.reload}
        savedVerdict
        truncated={Boolean(knownUsers.data?.eligibility_truncated)}
        onChange={(next) => setPolicy({ users: next })}
      />

      <AllowList
        level="teams"
        selected={teams}
        candidates={teamCandidates}
        gated={gated}
        loading={structure.loading}
        error={structure.error}
        needToken={!tokenSaved}
        onChange={(next) => setPolicy({ teams: next })}
      />

      <AllowList
        level="organizations"
        selected={orgs}
        candidates={orgCandidates}
        gated={gated}
        loading={structure.loading}
        error={structure.error}
        needToken={!tokenSaved}
        onChange={(next) => setPolicy({ organizations: next })}
      />
    </>
  )
}

/** One level's allow list: tick what is discovered, or add an entry by hand.
 *
 *  Ticking a discovered row rather than typing a key, for the reason the model-policy tables work
 *  the same way: a team is keyed '<enterprise slug>/<numeric team id>', which nobody can be
 *  expected to type correctly, and a key with a typo in it is not invalid -- it simply never
 *  matches anybody, silently. Typing one by hand stays possible because a deployment with no
 *  enterprise administrator token has nothing to pick from and must still be configurable.
 */
function AllowList({
  level,
  selected,
  candidates,
  gated,
  loading,
  error,
  needToken,
  savedVerdict,
  truncated,
  onRefresh,
  onChange,
}: {
  level: 'users' | 'teams' | 'organizations'
  selected: string[]
  candidates: Candidate[] | null
  /** Whether key creation is gated at all. With the gate open every account is eligible. */
  gated: boolean
  loading: boolean
  error: string
  needToken?: boolean
  /** Whether this level's eligibility comes from the server's reading of the **saved** policy, in
   *  which case an unsaved key-policy edit is not reflected here and the page has to say so. */
  savedVerdict?: boolean
  /** Whether the server stopped short of evaluating every account at this level. */
  truncated?: boolean
  onRefresh?: () => void
  onChange: (next: string[]) => void
}) {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  const [query, setQuery] = useState('')
  const [showAll, setShowAll] = useState(false)

  const chosen = new Set(selected.map((s) => s.toLowerCase()))
  const isChosen = (key: string) => chosen.has(key.toLowerCase())

  const toggle = (key: string, on: boolean) => {
    if (on) {
      if (isChosen(key)) return
      onChange([...selected, key])
    } else {
      onChange(selected.filter((s) => s.toLowerCase() !== key.toLowerCase()))
    }
  }

  // Only accounts that may create an API key are offered, because the permission this page grants
  // is worthless to anybody else. `eligible === null` is an unknown verdict, not a denial, and is
  // offered: hiding a row the server could not evaluate would remove it from the configuration
  // silently. An entry that is **already listed** is likewise kept, so a policy change cannot
  // strand it out of reach.
  const all = candidates ?? []
  const offered = useMemo(
    () => all.filter((c) => c.eligible !== false || isChosen(c.key)),
    // Deliberately not keyed on `selected`: rows must not appear and disappear under the cursor
    // mid-edit, and nothing is missed by it because a newly tickable row was eligible already.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [candidates],
  )
  const excluded = all.length - offered.length

  // Allowed rows first, then the rest, alphabetically inside each band: the cap below must never
  // hide a row that is part of the current configuration.
  const needle = query.trim().toLowerCase()
  const rows = useMemo(() => {
    const matched = needle
      ? offered.filter(
          (c) =>
            c.key.toLowerCase().includes(needle) ||
            c.name.toLowerCase().includes(needle) ||
            (c.enterprise ?? '').toLowerCase().includes(needle),
        )
      : offered
    const rank = (c: Candidate) => (isChosen(c.key) ? 0 : c.eligible === false ? 2 : 1)
    return [...matched].sort(
      (a, b) =>
        rank(a) - rank(b) ||
        (a.enterprise ?? '').localeCompare(b.enterprise ?? '') ||
        a.name.localeCompare(b.name),
    )
    // Not keyed on `selected`: a re-sort on every tick would move rows under the cursor, and
    // nothing is missed because an untickable row is not in the list to begin with.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [offered, needle])

  const shown = showAll ? rows : rows.slice(0, VISIBLE)
  const hidden = rows.length - shown.length

  // An allowed entry the discovered structure does not contain: typed by hand, or from an
  // enterprise this token can no longer see. Listed rather than dropped, because an entry the
  // backend still honours must be visible on the page that owns it.
  const known = new Set(all.map((c) => c.key.toLowerCase()))
  const manual = selected.filter((s) => !known.has(s.toLowerCase()))

  const addManually = async () => {
    const key = await dialogs.prompt({
      title: t(`access.keyscope.${level}.addTitle`),
      message: t(`access.keyscope.${level}.prompt`),
      label: t(`access.keyscope.${level}.key`),
      placeholder:
        level === 'teams' ? 'your-enterprise/14501973' : level === 'users' ? 'octocat' : 'your-org',
      mono: true,
      validate: (v) => {
        if (isChosen(v)) return t('access.keyscope.exists', { name: v })
        // Checked before the dialog closes: the team key format is easy to get wrong, and fixing
        // it in the field beats retyping the whole thing after a toast.
        if (level === 'teams' && !/^[^/]+\/[^/]+$/.test(v))
          return t('access.keyscope.badKey', { name: v })
        return null
      },
    })
    if (!key) return
    onChange([...selected, key])
  }

  return (
    <div className="panel">
      <div className="panel-head">
        {t(`access.keyscope.${level}.title`)}
        {selected.length ? (
          <span className="badge">{t('access.keyscope.allowedCount', { count: selected.length })}</span>
        ) : (
          <span className="badge" title={t('access.keyscope.notConsultedHint')}>
            {t('access.keyscope.notConsulted')}
          </span>
        )}
        <span className="spacer" />
        {rows.length > VISIBLE && (
          <input
            type="text"
            className="head-search"
            value={query}
            placeholder={t(`access.keyscope.${level}.search`)}
            onChange={(e) => setQuery(e.target.value)}
          />
        )}
        {onRefresh && (
          <button className="btn subtle sm" onClick={onRefresh}>
            {t('common.refresh')}
          </button>
        )}
        <button className="btn ghost sm" onClick={addManually}>
          {t('access.keyscope.addManually')}
        </button>
      </div>
      <div className="panel-body">
        <p className="panel-note" style={{ marginTop: 0 }}>
          <Trans
            i18nKey={`access.keyscope.${level}.lead`}
            components={{ strong: <strong />, code: <code /> }}
          />
        </p>

        {/* What the key policy is doing to this list, stated once per table rather than per row */}
        <p className="panel-note">
          {gated ? (
            <>
              <span className="badge">{t('access.keyscope.gateOnBadge')}</span>{' '}
              <Trans i18nKey="access.keyscope.gateOn" components={{ strong: <strong /> }} />
            </>
          ) : (
            <>
              <span className="badge warn">{t('access.keyscope.gateOffBadge')}</span>{' '}
              <Trans i18nKey="access.keyscope.gateOff" components={{ strong: <strong /> }} />
            </>
          )}
          {savedVerdict && gated && <> {t('access.keyscope.savedVerdict')}</>}
        </p>

        {excluded > 0 && (
          <p className="panel-note">
            <span className="badge">{t('access.keyscope.excludedBadge')}</span>{' '}
            {t('access.keyscope.excluded', { hidden: excluded, total: all.length })}
          </p>
        )}

        {truncated && (
          <p className="panel-note">
            <span className="badge warn">{t('access.keyscope.partialBadge')}</span>{' '}
            {t('access.keyscope.partialUsers')}
          </p>
        )}

        {needToken ? (
          <p className="panel-note">
            <span className="badge warn">{t('access.keyscope.noStructure')}</span>{' '}
            <Trans i18nKey="access.keyscope.needToken" components={{ strong: <strong /> }} />
          </p>
        ) : error ? (
          <p className="panel-note">
            <span className="badge error">{t('common.error')}</span> {error}
          </p>
        ) : loading && candidates === null ? (
          <div className="empty">{t('common.loading')}</div>
        ) : !all.length ? (
          <div className="empty">{t(`access.keyscope.${level}.none`)}</div>
        ) : !offered.length ? (
          /* Discovered, but every one of them is barred from creating a key. A different state from
             "nothing was discovered", and saying the latter would send an administrator off to
             debug the token instead of the key policy. */
          <div className="empty">{t(`access.keyscope.${level}.noneEligible`)}</div>
        ) : !rows.length ? (
          <div className="empty">{t('access.keyscope.noMatch', { query: query.trim() })}</div>
        ) : null}

        {(shown.length > 0 || manual.length > 0) && (
          <table>
            <colgroup>
              <col style={{ width: '36%' }} />
              <col style={{ width: '26%' }} />
              <col style={{ width: '22%' }} />
              <col style={{ width: '16%' }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t(`access.keyscope.${level}.key`)}</th>
                <th>{t('access.keyscope.table.source')}</th>
                <th>{t('access.keyscope.table.keyPolicy')}</th>
                <th>{t('access.keyscope.table.allow')}</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((c) => (
                <tr key={c.key}>
                  <td className={level === 'teams' ? '' : 'mono truncate'}>
                    {c.name}
                    {/* The stored key under the display name: a team name does not identify a
                        team, and an administrator comparing this page with config.yaml needs the
                        id itself. */}
                    {level === 'teams' && <div className="dim mono">{c.key}</div>}
                  </td>
                  <td className="truncate dim">{c.enterprise ?? t('access.keyscope.signedIn')}</td>
                  <td>
                    {c.eligible === null ? (
                      <span className="badge" title={t('access.keyscope.reason.unknown')}>
                        {t('access.keyscope.unknownKeys')}
                      </span>
                    ) : c.eligible ? (
                      <span className="badge ok">{t('access.keyscope.mayCreateKeys')}</span>
                    ) : (
                      <span
                        className="badge warn"
                        title={t(`access.keyscope.reason.${c.reason ?? 'orgNotAllowed'}`)}
                      >
                        {t('access.keyscope.mayNotCreateKeys')}
                      </span>
                    )}
                  </td>
                  <td>
                    {/* An ineligible account cannot be **newly** allowed, but one that is already
                        allowed stays tickable: that is the only way to clear an entry left behind
                        by a key-policy change. */}
                    <input
                      type="checkbox"
                      checked={isChosen(c.key)}
                      disabled={c.eligible === false && !isChosen(c.key)}
                      title={
                        c.eligible === false && !isChosen(c.key)
                          ? t(`access.keyscope.reason.${c.reason ?? 'orgNotAllowed'}`)
                          : undefined
                      }
                      onChange={(e) => toggle(c.key, e.target.checked)}
                    />
                  </td>
                </tr>
              ))}
              {manual.map((key) => (
                <tr key={key}>
                  <td className="mono truncate">{key}</td>
                  <td>
                    <span className="badge" title={t('access.keyscope.manualHint')}>
                      {t('access.keyscope.manual')}
                    </span>
                  </td>
                  <td className="dim">{t('access.keyscope.unknownKeys')}</td>
                  <td>
                    <input type="checkbox" checked onChange={() => toggle(key, false)} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}

        {/* Never silent about the cap: a shortened list that says nothing reads as the whole list. */}
        {hidden > 0 && (
          <div className="list-footer" style={{ padding: '8px 0 0', border: 0 }}>
            <span className="dim">
              {t('access.keyscope.hidden', { shown: shown.length, total: rows.length })}
            </span>
            <span className="spacer" />
            <button className="btn ghost sm" onClick={() => setShowAll(true)}>
              {t('access.keyscope.showAll', { total: rows.length })}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

/** The enterprise structure the team and organization pickers are drawn from. Cache-first: the
 *  refresh button that goes to GitHub lives on the Key policy tab, which owns the token. */
function useStructure(tokenSaved: boolean): {
  discovered: DiscoveredEnterprise[] | null
  loading: boolean
  error: string
} {
  const [discovered, setDiscovered] = useState<DiscoveredEnterprise[] | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!tokenSaved) return
    let live = true
    setLoading(true)
    discoverEnterprises()
      .then((r) => {
        if (!live) return
        setDiscovered(r.enterprises)
        setError('')
      })
      .catch((e) => live && setError(String(e)))
      .finally(() => live && setLoading(false))
    return () => {
      live = false
    }
  }, [tokenSaved])

  return { discovered, loading, error }
}

/** Everyone who has signed in, so the user level is a tick list rather than a typed login.
 *
 *  Asked for **with** the key-creation verdict, which the plain model-policy table does not need:
 *  this page filters by it, and computing it here from the config is not possible -- whether a
 *  login may create a key is a GitHub membership question, and only the server can answer it.
 */
function useSignedInUsers(): {
  data: { users: KnownUser[]; eligibility_truncated?: boolean } | null
  error: string
  reload: () => void
} {
  const [data, setData] = useState<{ users: KnownUser[]; eligibility_truncated?: boolean } | null>(
    null,
  )
  const [error, setError] = useState('')

  const reload = useCallback(() => {
    getSignedInUsers(true)
      .then((r) => {
        setData(r)
        setError('')
      })
      .catch((e) => setError(String(e)))
  }, [])

  useEffect(() => {
    reload()
  }, [reload])

  return { data, error, reload }
}
