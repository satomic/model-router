import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

const source = readFileSync(new URL('../src/pages/topology/graph.ts', import.meta.url), 'utf8')
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2020 } }).outputText
const { buildGraph, policyNeighborhood, neighborhoodPositions } = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString('base64')}`)
const snapshot = {
  config: {
    models: { cheap: { provider: 'primary' }, premium: { provider: 'other' } },
    providers: { primary: { api_type: 'openai' }, other: { api_type: 'anthropic' } },
    default_provider: 'primary',
    auth: {
      github: {}, admin_logins: ['admin'], allow_any_github_user: false,
      key_policy: { enabled: true, enterprises: { acme: { enabled: false, organizations: ['engineering'], teams: [42] } } },
      key_scope_policy: { enabled: true, users: ['alice'], organizations: ['engineering'] },
    },
    model_groups: { starter: ['cheap'], locked: [] },
    model_policy: { enabled: true, default_group: 'locked', users: { alice: 'starter' }, teams: { 'acme/42': 'starter' } },
  },
  enterprises: [{ slug: 'acme', name: 'Acme', organizations: [{ login: 'engineering', name: 'Engineering' }], teams: [{ id: 42, name: 'Developers', slug: 'developers' }] }],
  users: { users: [{ login: 'alice', name: 'Alice', kind: 'github', can_create_key: null }] },
  keys: [{ id: 'key-one', name: 'Scoped', user_login: 'alice', key: 'must-not-appear', scope: { kind: 'api_types', api_types: ['openai'] }, disabled: true }],
}
const initial = buildGraph({ ...snapshot, users: null }, key => key)
assert.ok(!initial.nodes.some(item => item.kind === 'user' || item.kind === 'key'))
const membersOnly = buildGraph(snapshot, key => key)
assert.ok(!membersOnly.edges.some(edge => edge.source === 'user:alice' && edge.kind !== 'structure'))
snapshot.selectedLogin = 'alice'
snapshot.memberScope = 'organization:engineering'
const graph = buildGraph(snapshot, key => key)
const neighborhood = policyNeighborhood(graph, 'group:starter')
assert.ok(neighborhood.edges.every(edge => edge.source === 'group:starter' || edge.target === 'group:starter'))
assert.ok(!neighborhood.nodes.some(item => item.id === 'model:premium'))
const localPositions = neighborhoodPositions(neighborhood, 'group:starter')
assert.ok(localPositions.get('user:alice').y < localPositions.get('group:starter').y)
assert.ok(localPositions.get('model:cheap').y > localPositions.get('group:starter').y)
const boxes = [...localPositions.values()]
for (let first = 0; first < boxes.length; first++) for (let second = first + 1; second < boxes.length; second++) {
  assert.ok(Math.abs(boxes[first].x - boxes[second].x) >= 220 || Math.abs(boxes[first].y - boxes[second].y) >= 82)
}
const organizationView = policyNeighborhood(graph, 'organization:engineering')
assert.equal(organizationView.edges.filter(edge => edge.source === 'enterprise:acme').length, 1)
assert.ok(organizationView.edges.find(edge => edge.source === 'enterprise:acme').label.includes('configuredScope'))
const repeated = { ...graph, edges: [...graph.edges, { ...graph.edges.find(edge => edge.source === 'user:alice' && edge.target === 'group:starter'), id: 'extra-contribution', label: 'applied contribution' }] }
const combined = policyNeighborhood(repeated, 'group:starter').edges.filter(edge => edge.source === 'user:alice')
assert.equal(combined.length, 1)
assert.ok(combined[0].label.includes('applied contribution'))
const node = id => graph.nodes.find(node => node.id === id)
assert.equal(node('team:acme/42').label, 'Developers')
assert.equal(node('scope').summary, 'AND')
assert.equal(node('user:alice').summary, 'createKey: unknown')
assert.equal(node('group:locked').details[1][1], 'emptyGroup')
assert.ok(graph.edges.filter(edge => edge.target === 'create').every(edge => edge.inactive))
assert.ok(graph.edges.some(edge => edge.source === 'admins:*' && edge.target === 'login'))
assert.ok(graph.edges.some(edge => edge.source === 'everyone:*' && edge.target === 'group:locked'))
assert.ok(graph.edges.some(edge => edge.source === 'bypass' && edge.target === 'model:premium'))
assert.deepEqual(graph.edges.filter(edge => edge.source === 'key:key-one').map(edge => edge.target), ['model:cheap'])
assert.ok(graph.edges.filter(edge => edge.source === 'key:key-one').every(edge => edge.inactive))
assert.ok(!JSON.stringify(graph).includes('must-not-appear'))
assert.ok(graph.edges.some(edge => edge.source === 'organization:engineering' && edge.target === 'user:alice'))
assert.ok(!policyNeighborhood(graph, 'user:alice').nodes.some(item => item.id === 'model:premium'))
assert.equal(new Set(graph.nodes.map(item => item.id)).size, graph.nodes.length)
assert.ok(graph.edges.every(edge => node(edge.source) && node(edge.target)))
snapshot.config.model_policy.enabled = false
snapshot.config.auth.key_policy.enabled = false
snapshot.config.auth.key_scope_policy = { enabled: true }
const disabled = buildGraph(snapshot, key => key)
assert.ok(disabled.edges.filter(edge => edge.source.startsWith('group:')).every(edge => edge.inactive))
assert.ok(disabled.edges.some(edge => edge.source === 'everyone:*' && edge.target === 'create' && !edge.inactive))
assert.equal(disabled.nodes.find(item => item.id === 'scope').summary, 'grantsNobody')
console.log('Topology checks passed: policy semantics, scopes, unknown users, privacy, local relationships and non-overlapping layout.')
const statusSource = readFileSync(new URL('../src/pages/topology/status.ts', import.meta.url), 'utf8')
const statusCode = ts.transpileModule(statusSource, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2020 } }).outputText
const { dataIssues } = await import(`data:text/javascript;base64,${Buffer.from(statusCode).toString('base64')}`)
const cache = { token_configured: true, token_matches: true, stale: false, refresh_seconds: 3600, structure: { fetched_at: 123, error: '' }, members: { scopes: [] } }
assert.deepEqual(dataIssues([], cache), [])
const issues = dataIssues([{ slug: 'large-enterprise', organizations: Array(200).fill({}), organizations_total: 997, organizations_truncated: true, organizations_error: null, teams: [], teams_error: 'GitHub returned 404' }], cache, 123)
assert.equal(issues.length, 2)
assert.deepEqual(issues[0].values, { loaded: 200, total: 997 })
assert.equal(issues[0].scope, 'large-enterprise')
assert.equal(issues[1].reason, 'GitHub returned 404')
assert.equal(issues[1].fetchedAt, 123)
const cacheIssues = dataIssues([], { ...cache, stale: true, token_matches: false, members: { scopes: [{ key: 'org:engineering', count: 50, truncated: true, error: 'rate limited', fetched_at: 456 }] } })
assert.deepEqual(cacheIssues.map(issue => issue.key), ['statusTokenMismatch', 'statusStale', 'statusMembersTruncated', 'statusMembersError'])
assert.equal(cacheIssues[2].scope, 'org:engineering')
assert.equal(cacheIssues[3].reason, 'rate limited')
console.log('Data status checks passed: enterprise counts, separate failures, timestamps and per-scope cache diagnostics.')