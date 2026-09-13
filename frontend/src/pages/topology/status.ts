import type { CacheStatus, DiscoveredEnterprise } from '../../api'

export interface DataIssue {
  scope: string
  key: string
  values?: Record<string, string | number>
  reason?: string
  fetchedAt?: number
}

export function dataIssues(enterprises: DiscoveredEnterprise[], cache: CacheStatus | null, discoveryAt?: number): DataIssue[] {
  const issues: DataIssue[] = []
  for (const enterprise of enterprises) {
    const common = { scope: enterprise.slug, fetchedAt: discoveryAt }
    if (enterprise.organizations_truncated) issues.push({ ...common, key: 'statusOrgTruncated', values: { loaded: enterprise.organizations.length, total: enterprise.organizations_total } })
    if (enterprise.organizations_error) issues.push({ ...common, key: 'statusOrgError', values: { loaded: enterprise.organizations.length }, reason: enterprise.organizations_error })
    if (enterprise.teams_error) issues.push({ ...common, key: 'statusTeamError', values: { loaded: enterprise.teams.length }, reason: enterprise.teams_error })
  }
  if (!cache) return issues
  if (cache.token_configured === false) issues.push({ scope: 'GitHub', key: 'statusNoToken' })
  else if (cache.token_matches === false) issues.push({ scope: 'GitHub', key: 'statusTokenMismatch' })
  if (cache.stale) issues.push({ scope: 'GitHub', key: 'statusStale', values: { interval: cache.refresh_seconds }, fetchedAt: cache.structure.fetched_at })
  if (cache.error) issues.push({ scope: 'GitHub', key: 'statusCacheError', reason: cache.error })
  if (cache.structure.error) issues.push({ scope: 'GitHub', key: 'statusStructureError', reason: cache.structure.error, fetchedAt: cache.structure.fetched_at })
  for (const scope of cache.members.scopes) {
    const common = { scope: scope.key, fetchedAt: scope.fetched_at }
    if (scope.truncated) issues.push({ ...common, key: 'statusMembersTruncated', values: { loaded: scope.count } })
    if (scope.error) issues.push({ ...common, key: 'statusMembersError', values: { loaded: scope.count }, reason: scope.error })
  }
  return issues
}