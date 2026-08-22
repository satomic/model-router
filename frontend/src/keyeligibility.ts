import type { DiscoveredEnterprise, KeyPolicy } from './api'

/** One bindable or allowable scope: an enterprise team, or an organization. */
export interface ScopeCandidate {
  /** The key stored in config: an organization login, or '<enterprise slug>/<numeric team id>'. */
  key: string
  /** What to show a human: the organization login, or the team's display name. */
  name: string
  /** Which enterprise it came from, so two identically named teams stay distinguishable. */
  enterprise: string
  /** Whether members of this scope may create an API key at all, under the current key policy. */
  eligible: boolean
  /** Why not, when they may not. Each page maps this slug into its own translation namespace. */
  reason?: KeyPolicyReason
}

export type KeyPolicyReason = 'entOff' | 'orgNotAllowed' | 'teamNotAllowed'

/** Build the scope list from the discovered structure, tagged with who may create an API key.
 *
 *  Shared by the model-policy bindings tables and the key-scope allow lists rather than written
 *  twice, because the rule has three subtleties that would silently drift apart: the enterprise
 *  master switch overrides both lists, `allow_all_orgs` makes the organization list moot, and a
 *  team is keyed by numeric id while an organization is keyed by login.
 *
 *  The filter is the point rather than a nicety. A scope whose members cannot create a key grants
 *  nothing on either page: without a key they never reach /v1/chat/completions, so neither the
 *  model group bound to them nor the permission to narrow a key they cannot create means anything.
 *  So `eligible` decides whether a table offers a row at all, and the caller says how many it
 *  dropped. The reason is still recorded per candidate, because a scope that is **already
 *  configured** survives that filter and has to explain itself.
 *
 *  With the key policy switched off every discovered scope is eligible: the gate is open, so
 *  anybody who can sign in can create a key.
 */
export function scopeCandidates(
  scope: 'teams' | 'organizations',
  discovered: DiscoveredEnterprise[],
  keyPolicy: KeyPolicy,
): ScopeCandidate[] {
  const gated = Boolean(keyPolicy.enabled)
  const rules = keyPolicy.enterprises ?? {}
  const out: ScopeCandidate[] = []

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
