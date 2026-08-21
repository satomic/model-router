import { useMemo, useState } from 'react'
import { useTranslation, Trans } from 'react-i18next'
import type { DiscoveredEnterprise, KeyPolicy } from '../../api'
import { useDialogs } from '../../components/Dialog'

/** How many rows are rendered before the list has to be asked for in full.
 *
 *  Not cosmetic. A GitHub enterprise can own a four-figure number of organizations, and a table
 *  that renders every one of them is both unusable and slow -- the enterprise on hand here has
 *  998. The cap is safe because the ordering below puts everything an administrator came for
 *  (bound rows, then bindable ones) above it, and the count is always stated.
 */
const VISIBLE = 20

/** One bindable scope: an organization, or an enterprise team. */
interface Candidate {
  /** The binding key stored in config: an organization login, or '<enterprise slug>/<team id>'. */
  key: string
  /** What to show a human -- the organization login, or the team's display name. */
  name: string
  /** Which enterprise it came from, so two identically named teams stay distinguishable. */
  enterprise: string
  /** Whether members of this scope may create an API key at all, under the current key policy. */
  eligible: boolean
  /** Why not, when they may not. A translation key under `policy.bindings.`. */
  reason?: 'entOff' | 'orgNotAllowed' | 'teamNotAllowed'
}

/** Build the bindable list from the discovered structure, filtered by who may create an API key.
 *
 *  The filter is the point rather than a nicety: a model group bound to a scope whose members
 *  cannot create a key grants nothing, because without a key they never reach
 *  /v1/chat/completions. So `eligible` is what decides whether the table offers a row at all; the
 *  caller drops the ineligible ones and says how many it dropped. The reason is still recorded per
 *  candidate, because a scope that is **already bound** survives that filter and has to explain
 *  itself.
 *
 *  With the key policy switched off every discovered scope is eligible: the gate is open, so
 *  anybody who can sign in can create a key.
 */
function candidatesFor(
  scope: 'teams' | 'organizations',
  discovered: DiscoveredEnterprise[],
  keyPolicy: KeyPolicy,
): Candidate[] {
  const gated = Boolean(keyPolicy.enabled)
  const rules = keyPolicy.enterprises ?? {}
  const out: Candidate[] = []

  for (const ent of discovered) {
    const rule = rules[ent.slug] ?? {}
    const entOn = Boolean(rule.enabled)
    const allowedOrgs = (rule.organizations ?? []).map(String)
    const allowedTeams = (rule.teams ?? []).map(String)

    if (scope === 'organizations') {
      for (const org of ent.organizations) {
        const allowed = Boolean(rule.allow_all_orgs) || allowedOrgs.includes(org.login)
        out.push({
          key: org.login,
          name: org.login,
          enterprise: ent.name,
          eligible: !gated || (entOn && allowed),
          reason: !gated ? undefined : !entOn ? 'entOff' : allowed ? undefined : 'orgNotAllowed',
        })
      }
    } else {
      for (const team of ent.teams) {
        const allowed = allowedTeams.includes(String(team.id))
        out.push({
          // The numeric id, not the slug: this is the form the backend's team lookup needs.
          key: `${ent.slug}/${team.id}`,
          name: team.name,
          enterprise: ent.name,
          eligible: !gated || (entOn && allowed),
          reason: !gated ? undefined : !entOn ? 'entOff' : allowed ? undefined : 'teamNotAllowed',
        })
      }
    }
  }
  return out
}

/** Team and organization bindings, picked from the structure discovered on the Access control page.
 *
 *  Dropdowns over a discovered list rather than typed keys, for the same reason the signed-in users
 *  table works that way: a team binding is keyed '<enterprise slug>/<numeric team id>', which
 *  nobody can be expected to type correctly, and a key with a typo in it silently binds nothing at
 *  all -- it is not invalid, it just never matches anybody.
 *
 *  Typing one by hand is still possible, but only as the fallback it should be: with no enterprise
 *  administrator token configured there is no structure to pick from, and a deployment in that
 *  state must still be configurable.
 */
export default function ScopeBindings({
  scope,
  table,
  groupNames,
  groups,
  onChange,
  discovered,
  keyPolicy,
  tokenSaved,
  loading,
  error,
}: {
  scope: 'teams' | 'organizations'
  table?: Record<string, string>
  groupNames: string[]
  groups: Record<string, string[]>
  onChange: (table: Record<string, string>) => void
  discovered: DiscoveredEnterprise[] | null
  keyPolicy: KeyPolicy
  /** Whether an enterprise administrator token is saved -- discovery is impossible without one. */
  tokenSaved: boolean
  loading: boolean
  error: string
}) {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  const [query, setQuery] = useState('')
  const [showAll, setShowAll] = useState(false)

  const bindings = table ?? {}
  const all = useMemo(
    () => candidatesFor(scope, discovered ?? [], keyPolicy),
    [scope, discovered, keyPolicy],
  )
  const known = new Set(all.map((c) => c.key))

  // Only what the key policy allows is offered. An **already bound** scope stays in the table even
  // once it has become ineligible: the backend still enforces that binding, and a row that vanishes
  // from the page owning it leaves an administrator no way to clear it.
  const offered = useMemo(
    () => all.filter((c) => c.eligible || bindings[c.key]),
    // Deliberately not keyed on `bindings`, for the same reason the sort below is not: rows must not
    // appear and disappear under the cursor mid-edit. Nothing is missed by it either, since a row
    // that can be newly bound was eligible already and is therefore in the list.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [all],
  )
  const excluded = all.length - offered.length

  // Ordered so the cap below never hides anything an administrator is looking for: what is already
  // bound first (those rows are the current configuration and must always be visible), then what
  // can be bound, then the rest. Within each band, alphabetical -- a discovery order nobody chose
  // is worse than no order at all when the list runs to hundreds.
  const needle = query.trim().toLowerCase()
  const candidates = useMemo(() => {
    const matched = needle
      ? offered.filter(
          (c) =>
            c.key.toLowerCase().includes(needle) ||
            c.name.toLowerCase().includes(needle) ||
            c.enterprise.toLowerCase().includes(needle),
        )
      : offered
    const rank = (c: Candidate) => (bindings[c.key] ? 0 : c.eligible ? 1 : 2)
    return [...matched].sort(
      (a, b) => rank(a) - rank(b) || a.enterprise.localeCompare(b.enterprise) ||
        a.name.localeCompare(b.name),
    )
    // `bindings` is read through the ranking, and a re-sort on every keystroke of a group select
    // would make rows jump under the cursor -- so the sort deliberately does not depend on it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [offered, needle])

  const shown = showAll ? candidates : candidates.slice(0, VISIBLE)
  const hidden = candidates.length - shown.length

  // Discovery itself can be short of the truth: an enterprise with 998 organizations returns only
  // the first page-load of them. Worth saying on this page and not only on Access control, because
  // this is where somebody searches for an organization and fails to find it -- and the answer is
  // "type it by hand", not "it does not exist".
  const partial =
    scope === 'organizations' && (discovered ?? []).some((e) => e.organizations_truncated)

  // A binding whose scope is not in the discovered structure: typed by hand, or from an enterprise
  // this token can no longer see. Listed after the candidates rather than dropped, because a
  // binding that vanishes from the page while still being enforced by the backend is the worst of
  // both worlds.
  const orphans = Object.keys(bindings).filter((k) => !known.has(k))
  const boundCount = Object.keys(bindings).length

  const setBinding = (key: string, group: string) => {
    const next = { ...bindings }
    if (group) next[key] = group
    else delete next[key]
    onChange(next)
  }

  const addManually = async () => {
    const key = await dialogs.prompt({
      title: t(`policy.${scope}.addTitle`),
      message: t(`policy.${scope}.prompt`),
      label: t(`policy.${scope}.key`),
      placeholder: scope === 'teams' ? 'your-enterprise/14501973' : 'your-org',
      mono: true,
      // Both checks run before the dialog closes. The malformed-team-key case especially: the
      // format is easy to get wrong, and correcting it in the field beats retyping the whole key
      // after a toast.
      validate: (v) => {
        if (bindings[v]) return t('policy.bindingExists', { name: v })
        if (scope === 'teams' && !/^[^/]+\/[^/]+$/.test(v)) return t('policy.teams.badKey', { name: v })
        return null
      },
    })
    if (!key) return
    setBinding(key, groupNames[0] ?? '')
  }

  const groupSelect = (key: string, disabled: boolean, title?: string) => (
    <select
      value={bindings[key] ?? ''}
      disabled={disabled}
      title={title}
      onChange={(e) => setBinding(key, e.target.value)}
    >
      <option value="">{t('policy.noBinding')}</option>
      {groupNames.map((g) => (
        <option key={g} value={g}>
          {t('policy.groupOption', { name: g, count: (groups[g] ?? []).length })}
        </option>
      ))}
    </select>
  )

  return (
    <div className="panel">
      <div className="panel-head">
        {t(`policy.${scope}.title`)}
        <span className="badge">{t('policy.bindingCount', { count: boundCount })}</span>
        <span className="spacer" />
        {/* Only once the list is long enough to need it: a search box over three rows is clutter. */}
        {offered.length > VISIBLE && (
          <input
            type="text"
            className="head-search"
            value={query}
            placeholder={t(`policy.${scope}.search`)}
            onChange={(e) => setQuery(e.target.value)}
          />
        )}
        <button className="btn ghost sm" onClick={addManually} disabled={!groupNames.length}>
          {t('policy.bindings.addManually')}
        </button>
      </div>
      <div className="panel-body">
        <p className="panel-note" style={{ marginTop: 0 }}>
          <Trans
            i18nKey={`policy.${scope}.lead`}
            components={{ strong: <strong />, code: <code /> }}
          />
        </p>

        {/* What the key policy is doing to this list, stated once per table rather than per row */}
        <p className="panel-note">
          {keyPolicy.enabled ? (
            <>
              <span className="badge">{t('policy.bindings.gateOnBadge')}</span>{' '}
              <Trans i18nKey="policy.bindings.gateOn" components={{ strong: <strong /> }} />
            </>
          ) : (
            <>
              <span className="badge warn">{t('policy.bindings.gateOffBadge')}</span>{' '}
              <Trans i18nKey="policy.bindings.gateOff" components={{ strong: <strong /> }} />
            </>
          )}
        </p>

        {excluded > 0 && (
          <p className="panel-note">
            <span className="badge">{t('policy.bindings.excludedBadge')}</span>{' '}
            {t('policy.bindings.excluded', { hidden: excluded, total: all.length })}
          </p>
        )}

        {partial && (
          <p className="panel-note">
            <span className="badge warn">{t('policy.bindings.partialBadge')}</span>{' '}
            {t('policy.bindings.partial', {
              listed: (discovered ?? []).reduce((n, e) => n + e.organizations.length, 0),
              total: (discovered ?? []).reduce((n, e) => n + e.organizations_total, 0),
            })}
          </p>
        )}

        {!groupNames.length && <div className="empty">{t('policy.bindings.noGroupsYet')}</div>}

        {!tokenSaved ? (
          <p className="panel-note" style={{ marginBottom: 0 }}>
            <span className="badge warn">{t('policy.bindings.noStructure')}</span>{' '}
            <Trans i18nKey="policy.bindings.needToken" components={{ strong: <strong /> }} />
          </p>
        ) : error ? (
          <p className="panel-note" style={{ marginBottom: 0 }}>
            <span className="badge error">{t('policy.bindings.discoverFailed')}</span> {error}
          </p>
        ) : loading && !discovered ? (
          <div className="empty">{t('policy.bindings.loading')}</div>
        ) : !all.length ? (
          <div className="empty">{t(`policy.${scope}.noneDiscovered`)}</div>
        ) : !offered.length ? (
          <div className="empty">{t(`policy.${scope}.noneAllowed`)}</div>
        ) : !candidates.length ? (
          /* A search that matched nothing is a different state from "nothing was discovered", and
             saying the latter here would send an administrator off to debug the token. */
          <div className="empty">{t('policy.bindings.noMatch', { query: query.trim() })}</div>
        ) : null}

        {(candidates.length > 0 || orphans.length > 0) && (
          <table>
            <colgroup>
              <col style={{ width: '34%' }} />
              <col style={{ width: '22%' }} />
              <col style={{ width: '18%' }} />
              <col style={{ width: '26%' }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t(`policy.${scope}.key`)}</th>
                <th>{t('policy.table.enterprise')}</th>
                <th>{t('policy.table.keyPolicy')}</th>
                <th>{t('policy.table.group')}</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((c) => {
                const bound = Boolean(bindings[c.key])
                return (
                  <tr key={c.key}>
                    <td className={scope === 'teams' ? '' : 'mono truncate'}>
                      {c.name}
                      {/* The stored key under the display name: an admin comparing this page with
                          config.yaml needs to see the id, and the name alone does not identify a
                          team -- two enterprises may each have a "Platform". */}
                      {scope === 'teams' && <div className="dim mono">{c.key}</div>}
                    </td>
                    <td className="truncate dim">{c.enterprise}</td>
                    <td>
                      {c.eligible ? (
                        <span className="badge ok">{t('policy.bindings.mayCreateKeys')}</span>
                      ) : (
                        <span
                          className="badge warn"
                          title={t(`policy.bindings.${c.reason ?? 'orgNotAllowed'}`)}
                        >
                          {t('policy.bindings.mayNotCreateKeys')}
                        </span>
                      )}
                    </td>
                    <td>
                      {/* An ineligible scope cannot take a **new** binding, but an existing one
                          stays editable: that is the only way to clear a binding left behind by a
                          key-policy change. */}
                      {groupSelect(
                        c.key,
                        !c.eligible && !bound,
                        c.eligible ? undefined : t(`policy.bindings.${c.reason ?? 'orgNotAllowed'}`),
                      )}
                    </td>
                  </tr>
                )
              })}
              {orphans.map((key) => (
                <tr key={key}>
                  <td className="mono truncate">{key}</td>
                  <td className="dim">—</td>
                  <td>
                    <span className="badge" title={t('policy.bindings.orphanHint')}>
                      {t('policy.bindings.orphan')}
                    </span>
                  </td>
                  <td>{groupSelect(key, false)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}

        {/* Never silent about the cap: a truncated list that says nothing reads as the whole list,
            and an administrator would conclude the organization they are looking for was not
            discovered at all. */}
        {hidden > 0 && (
          <div className="list-footer" style={{ padding: '8px 0 0', border: 0 }}>
            <span className="dim">
              {t('policy.bindings.hidden', { shown: shown.length, total: candidates.length })}
            </span>
            <span className="spacer" />
            <button className="btn ghost sm" onClick={() => setShowAll(true)}>
              {t('policy.bindings.showAll', { total: candidates.length })}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
