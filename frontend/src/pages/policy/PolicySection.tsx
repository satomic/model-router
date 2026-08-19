import { useCallback, useEffect, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import {
  discoverEnterprises,
  getSignedInUsers,
  type DiscoveredEnterprise,
  type KnownUser,
  type ModelPolicy,
} from '../../api'
import { useDialogs } from '../../components/Dialog'
import { formatDateTime } from '../../i18n/format'
import ScopeBindings from './ScopeBindings'
import type { PolicyProps } from './types'

/** Model policy: named model groups, and which group each user / team / organization gets.
 *
 *  Two things about the semantics drive the whole layout, and both are stated on the page rather
 *  than only in the docs, because neither is guessable from a table of bindings:
 *
 *  - Scopes resolve as a **union**. A binding can only widen a caller's list, never narrow it, so
 *    there is no ordering to configure and no precedence column to render.
 *  - Three states differ. Switched off means no restriction; switched on with nothing bound still
 *    means no restriction (so turning it on before filling the tables in cannot lock an operator
 *    out); bound to an **empty group** means no models at all. That last one is the point of
 *    supporting empty groups, and is how "a new user gets nothing until assigned" is expressed.
 */
export default function PolicySection({ cfg, set }: PolicyProps) {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  const groups = cfg.model_groups ?? {}
  const policy = cfg.model_policy ?? {}
  const groupNames = Object.keys(groups)
  const modelNames = Object.keys(cfg.models)
  const enabled = Boolean(policy.enabled)

  // The enterprise structure the team/organization pickers are drawn from. Read here rather than
  // inside each table so one request feeds both, and read from the **saved** config: this page
  // never edits `auth`, so `cfg.auth` is what the server has.
  const keyPolicy = cfg.auth?.key_policy ?? {}
  const tokenSaved = Boolean((keyPolicy.github_token ?? '').trim())
  const structure = useStructure(tokenSaved)

  const setPolicy = (patch: Partial<ModelPolicy>) => set({ model_policy: { ...policy, ...patch } })

  const setGroup = (name: string, members: string[]) =>
    set({ model_groups: { ...groups, [name]: members } })

  const addGroup = async () => {
    const name = await dialogs.prompt({
      title: t('policy.addGroupTitle'),
      message: t('policy.promptGroupName'),
      label: t('policy.groupNameLabel'),
      placeholder: 'starter',
      mono: true,
      validate: (v) => (groups[v] ? t('policy.groupExists', { name: v }) : null),
    })
    if (!name) return
    // Created empty on purpose: an empty group is a legitimate configuration, and starting from
    // "grants nothing" is safer than starting from "grants everything".
    set({ model_groups: { ...groups, [name]: [] } })
  }

  const renameGroup = async (from: string) => {
    const to = await dialogs.prompt({
      title: t('policy.renameGroupTitle'),
      message: t('policy.promptRenameGroup'),
      label: t('policy.groupNameLabel'),
      defaultValue: from,
      mono: true,
      validate: (v) => (v !== from && groups[v] ? t('policy.groupExists', { name: v }) : null),
    })
    if (!to || to === from) return
    // Key order is preserved so the cards do not jump around, and every binding is repointed in
    // the same patch -- a rename that left bindings behind would silently grant nothing.
    const renamed = Object.fromEntries(
      Object.entries(groups).map(([k, v]) => [k === from ? to : k, v]),
    )
    set({ model_groups: renamed, model_policy: repoint(policy, from, to) })
  }

  const removeGroup = async (name: string) => {
    const bound = countBindings(policy, name)
    if (
      bound &&
      !(await dialogs.confirm({
        title: t('policy.confirmDeleteBoundTitle', { name }),
        message: t('policy.confirmDeleteBound', { name, count: bound }),
        confirmLabel: t('common.delete'),
        danger: true,
      }))
    )
      return
    const { [name]: _omit, ...rest } = groups
    // Bindings to the deleted group are cleared rather than left dangling: the backend rejects a
    // binding naming an unknown group, so keeping them would make the config unsavable.
    set({ model_groups: rest, model_policy: repoint(policy, name, '') })
  }

  const toggleMember = (group: string, model: string, checked: boolean) => {
    const members = groups[group] ?? []
    setGroup(
      group,
      checked
        ? // Catalog order rather than click order, so the group reads the same as the Models page
          modelNames.filter((m) => m === model || members.includes(m))
        : members.filter((m) => m !== model),
    )
  }

  return (
    <>
      {/* -- Master switch and the default group -- */}
      <div className="panel">
        <div className="panel-head">
          {t('policy.title')}
          {enabled ? (
            <span className="badge ok">{t('policy.enabled')}</span>
          ) : (
            <span className="badge warn">{t('policy.disabled')}</span>
          )}
        </div>
        <div className="panel-body">
          <label className="check">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => setPolicy({ enabled: e.target.checked })}
            />
            {t('policy.toggle')}
          </label>
          <p className="panel-note" style={{ marginTop: 10 }}>
            <Trans
              i18nKey={enabled ? 'policy.enabledNote' : 'policy.disabledNote'}
              components={{ strong: <strong />, code: <code /> }}
            />
          </p>

          <label className="field" style={{ marginBottom: 0 }}>
            <span className="field-name">
              {t('policy.defaultGroup')}
              <span className="field-hint">{t('policy.defaultGroupHint')}</span>
            </span>
            <select
              value={policy.default_group ?? ''}
              onChange={(e) => setPolicy({ default_group: e.target.value })}
            >
              <option value="">{t('policy.noDefaultGroup')}</option>
              {groupNames.map((g) => (
                <option key={g} value={g}>
                  {t('policy.groupOption', { name: g, count: (groups[g] ?? []).length })}
                </option>
              ))}
            </select>
          </label>
          <p className="panel-note" style={{ marginBottom: 0 }}>
            <Trans i18nKey="policy.defaultGroupNote" components={{ strong: <strong /> }} />
          </p>
        </div>
      </div>

      {/* -- The groups themselves -- */}
      <div className="panel">
        <div className="panel-head">
          {t('policy.groupsTitle')}
          <span className="badge">{t('policy.groupCount', { count: groupNames.length })}</span>
          <span className="spacer" />
          <button className="btn ghost sm" onClick={addGroup}>
            {t('policy.addGroup')}
          </button>
        </div>
        <div className="panel-body" style={{ paddingBottom: groupNames.length ? 4 : undefined }}>
          <p className="panel-note" style={{ marginTop: 0 }}>
            <Trans i18nKey="policy.groupsLead" components={{ strong: <strong /> }} />
          </p>
          {!modelNames.length && <div className="empty">{t('policy.noModels')}</div>}
          {!groupNames.length ? (
            <div className="empty">{t('policy.noGroups')}</div>
          ) : (
            groupNames.map((name) => {
              const members = groups[name] ?? []
              const bound = countBindings(policy, name)
              return (
                <div className="provider-card" key={name}>
                  <div
                    className="panel-head"
                    style={{ borderBottom: 'none', paddingLeft: 0, paddingRight: 0 }}
                  >
                    <span className="name mono">{name}</span>
                    <span className={`badge ${members.length ? '' : 'warn'}`}>
                      {members.length
                        ? t('policy.memberCount', { count: members.length })
                        : t('policy.emptyGroup')}
                    </span>
                    {policy.default_group === name && (
                      <span className="badge ok">{t('policy.isDefaultGroup')}</span>
                    )}
                    {bound > 0 && (
                      <span className="badge">{t('policy.boundCount', { count: bound })}</span>
                    )}
                    <span className="spacer" />
                    <button className="btn subtle sm" onClick={() => setGroup(name, [...modelNames])}>
                      {t('policy.selectAll')}
                    </button>
                    <button className="btn subtle sm" onClick={() => setGroup(name, [])}>
                      {t('policy.selectNone')}
                    </button>
                    <button className="btn ghost sm" onClick={() => renameGroup(name)}>
                      {t('policy.rename')}
                    </button>
                    <button className="btn danger sm" onClick={() => removeGroup(name)}>
                      {t('common.delete')}
                    </button>
                  </div>
                  {modelNames.length > 0 && (
                    <div className="tiles">
                      {modelNames.map((m) => (
                        <label
                          key={m}
                          className="tile check"
                          title={cfg.models[m]?.description ?? m}
                        >
                          <input
                            type="checkbox"
                            checked={members.includes(m)}
                            onChange={(e) => toggleMember(name, m, e.target.checked)}
                          />
                          <span className="mono truncate">{m}</span>
                        </label>
                      ))}
                    </div>
                  )}
                  {!members.length && (
                    <p className="panel-note" style={{ marginBottom: 0 }}>
                      <Trans
                        i18nKey="policy.emptyGroupNote"
                        components={{ strong: <strong /> }}
                      />
                    </p>
                  )}
                </div>
              )
            })
          )}
        </div>
      </div>

      {/* -- Who has signed in, with a group per row -- */}
      <SignedInUsers
        policy={policy}
        groupNames={groupNames}
        groups={groups}
        onBind={(login, group) =>
          setPolicy({ users: withBinding(policy.users, login, group) })
        }
      />

      {/* -- Team and organization bindings, picked from the discovered enterprise structure -- */}
      <ScopeBindings
        scope="teams"
        table={policy.teams}
        groupNames={groupNames}
        groups={groups}
        onChange={(table) => setPolicy({ teams: table })}
        keyPolicy={keyPolicy}
        tokenSaved={tokenSaved}
        {...structure}
      />
      <ScopeBindings
        scope="organizations"
        table={policy.organizations}
        groupNames={groupNames}
        groups={groups}
        onChange={(table) => setPolicy({ organizations: table })}
        keyPolicy={keyPolicy}
        tokenSaved={tokenSaved}
        {...structure}
      />
    </>
  )
}

/** Fetch the enterprise / organization / team structure once, for both binding tables.
 *
 *  Read-only and cache-first: `/v1/access/discover` answers from the on-disk GitHub cache unless
 *  asked to refresh, so opening this page costs no GitHub calls. Refreshing the cache stays on
 *  Access control, where the token that fills it is configured -- two pages offering the same
 *  refresh button would be two places to look when the structure is stale.
 */
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

// -- Binding helpers ---------------------------------------------------------
// Bindings live in three sibling tables, so every edit is the same three-table walk. Written once
// here rather than inlined per table, where the "teams as well as users" case is easy to forget.

/** How many bindings (including the default group) point at `group`. */
function countBindings(policy: ModelPolicy, group: string): number {
  const tables = [policy.users, policy.teams, policy.organizations]
  return (
    (policy.default_group === group ? 1 : 0) +
    tables.reduce(
      (n, table) => n + Object.values(table ?? {}).filter((g) => g === group).length,
      0,
    )
  )
}

/** Repoint every binding from `from` to `to`; `to` of '' clears them (a rename, or a delete). */
function repoint(policy: ModelPolicy, from: string, to: string): ModelPolicy {
  const move = (table?: Record<string, string>) =>
    table &&
    Object.fromEntries(
      Object.entries(table)
        .map(([k, v]) => [k, v === from ? to : v])
        // An emptied binding is dropped rather than stored as '', so the saved config carries no
        // rows that look configured but grant nothing.
        .filter(([, v]) => v),
    )
  return {
    ...policy,
    default_group: policy.default_group === from ? to : policy.default_group,
    users: move(policy.users),
    teams: move(policy.teams),
    organizations: move(policy.organizations),
  }
}

/** Set or clear one row of a binding table. */
function withBinding(
  table: Record<string, string> | undefined,
  key: string,
  group: string,
): Record<string, string> {
  const next = { ...(table ?? {}) }
  if (group) next[key] = group
  else delete next[key]
  return next
}

/** Everyone who has ever signed in, each offering the group to bind them to.
 *
 *  Read from a durable registry rather than the session table: sessions are purged when they
 *  expire, so they can only answer "who is signed in right now". Binding a group by picking a row
 *  here is the point -- an admin should not have to ask a user to spell their GitHub login.
 */
function SignedInUsers({
  policy,
  groupNames,
  groups,
  onBind,
}: {
  policy: ModelPolicy
  groupNames: string[]
  groups: Record<string, string[]>
  onBind: (login: string, group: string) => void
}) {
  const { t } = useTranslation()
  const [users, setUsers] = useState<KnownUser[] | null>(null)
  const [error, setError] = useState('')

  const load = useCallback(() => {
    getSignedInUsers()
      .then((r) => {
        setUsers(r.users)
        setError('')
      })
      .catch((e) => setError(String(e)))
  }, [])

  useEffect(() => {
    load()
  }, [load])

  // The draft, not the server's answer: the picker has to show the unsaved edit.
  const bindings = policy.users ?? {}
  const bound = (login: string) =>
    Object.entries(bindings).find(([k]) => k.toLowerCase() === login.toLowerCase())?.[1] ?? ''

  return (
    <div className="panel">
      <div className="panel-head">
        {t('policy.usersTitle')}
        {users && <span className="badge">{t('policy.userCount', { count: users.length })}</span>}
        <span className="spacer" />
        <button className="btn subtle sm" onClick={load}>
          {t('common.refresh')}
        </button>
      </div>
      <div className="panel-body">
        <p className="panel-note" style={{ marginTop: 0 }}>
          <Trans i18nKey="policy.usersLead" components={{ strong: <strong /> }} />
        </p>
        {error ? (
          <p className="panel-note" style={{ marginBottom: 0 }}>
            <span className="badge error">{t('common.error')}</span> {error}
          </p>
        ) : !users ? (
          <div className="empty">{t('common.loading')}</div>
        ) : !users.length ? (
          <div className="empty">{t('policy.noUsers')}</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>{t('policy.table.login')}</th>
                <th>{t('policy.table.kind')}</th>
                <th>{t('policy.table.firstSeen')}</th>
                <th>{t('policy.table.lastSeen')}</th>
                <th>{t('policy.table.signIns')}</th>
                <th>{t('policy.table.group')}</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.login}>
                  <td className="mono">{u.login}</td>
                  <td>
                    <span className={`badge ${u.kind === 'local' ? 'warn' : ''}`}>
                      {t(`policy.kind.${u.kind}`, { defaultValue: u.kind })}
                    </span>
                  </td>
                  <td className="dim">{formatDateTime(u.first_seen * 1000)}</td>
                  <td className="dim">{formatDateTime(u.last_seen * 1000)}</td>
                  <td className="mono">{u.sign_ins}</td>
                  <td>
                    <select value={bound(u.login)} onChange={(e) => onBind(u.login, e.target.value)}>
                      <option value="">{t('policy.noBinding')}</option>
                      {groupNames.map((g) => (
                        <option key={g} value={g}>
                          {t('policy.groupOption', {
                            name: g,
                            count: (groups[g] ?? []).length,
                          })}
                        </option>
                      ))}
                    </select>
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

