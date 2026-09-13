import type { ApiKey, DiscoveredEnterprise, RouterConfig, SignedInUsers, TopologyUser } from '../../api'

export type Layer = 'identity' | 'policy' | 'model'
export type Category = 'access' | 'scope' | 'models' | 'keys'
export type GraphNode = {
  id: string
  label: string
  kind: string
  layer: Layer
  summary: string
  inactive?: boolean
  href?: string
  details: [string, string][]
}
export interface GraphEdge {
  id: string
  source: string
  target: string
  kind: 'structure' | Category
  label: string
  inactive?: boolean
}
export interface Snapshot {
  config: RouterConfig
  enterprises: DiscoveredEnterprise[]
  users: SignedInUsers | null
  keys: ApiKey[]
  selectedLogin?: string
  memberScope?: string
  userPolicy?: TopologyUser
}
export interface Graph { nodes: GraphNode[]; edges: GraphEdge[] }
export function policyNeighborhood(graph: Graph, id: string): Graph {
  const grouped = new Map<string, GraphEdge>()
  for (const edge of graph.edges) {
    if (edge.source !== id && edge.target !== id) continue
    const key = JSON.stringify([edge.source, edge.target, edge.kind, Boolean(edge.inactive)])
    const previous = grouped.get(key)
    if (!previous) grouped.set(key, { ...edge })
    else if (!previous.label.split('; ').includes(edge.label)) previous.label += `; ${edge.label}`
  }
  const edges = [...grouped.values()]
  const ids = new Set([id, ...edges.flatMap(edge => [edge.source, edge.target])])
  return { nodes: graph.nodes.filter(node => ids.has(node.id)), edges }
}

export function neighborhoodPositions(graph: Graph, id: string): Map<string, { x: number; y: number }> {
  const incoming = new Set(graph.edges.filter(edge => edge.target === id).map(edge => edge.source))
  const before = graph.nodes.filter(node => node.id !== id && incoming.has(node.id))
  const after = graph.nodes.filter(node => node.id !== id && !incoming.has(node.id))
  const columns = Math.max(1, Math.min(3, Math.max(before.length, after.length)))
  const positions = new Map<string, { x: number; y: number }>()
  const place = (nodes: GraphNode[], start: number) => nodes.forEach((node, index) => {
    const rowCount = Math.min(columns, nodes.length - Math.floor(index / columns) * columns)
    positions.set(node.id, { x: (columns - rowCount) * 130 + index % columns * 260, y: start + Math.floor(index / columns) * 110 })
  })
  place(before, 0)
  const centerY = Math.ceil(before.length / columns) * 110 + (before.length ? 70 : 0)
  positions.set(id, { x: (columns - 1) * 130, y: centerY })
  place(after, centerY + 160)
  return positions
}
type Translate = (key: string) => string

export function buildGraph(snapshot: Snapshot, text: Translate): Graph {
  const { config, enterprises, users, keys } = snapshot
  const selectedLogin = snapshot.selectedLogin?.toLowerCase()
  const auth = config.auth ?? { github: {} }
  const keyPolicy = auth.key_policy ?? {}
  const scopePolicy = auth.key_scope_policy ?? {}
  const modelPolicy = config.model_policy ?? {}
  const nodes = new Map<string, GraphNode>()
  const edges: GraphEdge[] = []
  const status = (enabled: boolean | undefined) => text(enabled ? 'enabled' : 'disabled')
  const add = (node: GraphNode) => { nodes.set(node.id, node); return node.id }
  const connect = (source: string, target: string, kind: GraphEdge['kind'], label: string, inactive = false) => {
    const id = JSON.stringify([source, target, kind, label])
    if (!edges.some(edge => edge.id === id)) edges.push({ id, source, target, kind, label, inactive })
  }
  const identity = (kind: string, name: string, label = name) => {
    const id = `${kind}:${name.toLowerCase()}`
    if (!nodes.has(id)) add({ id, label, kind, layer: 'identity', summary: text(kind), details: [[text('identifier'), name]] })
    return id
  }
  const everyone = identity('everyone', '*', text('everyone'))
  const admins = identity('admins', '*', text('admins'))
  for (const enterprise of enterprises) {
    const parent = identity('enterprise', enterprise.slug, enterprise.name || enterprise.slug)
    nodes.get(parent)!.details.push([text('dataState'), enterprise.organizations_error || enterprise.teams_error || (enterprise.organizations_truncated ? text('truncated') : text('discovered'))])
    for (const org of enterprise.organizations) connect(parent, identity('organization', org.login, org.login), 'structure', text('structure'))
    for (const team of enterprise.teams) connect(parent, identity('team', `${enterprise.slug}/${team.id}`, team.name || team.slug), 'structure', text('structure'))
  }
  for (const user of users?.users ?? []) {
    const id = identity('user', user.login)
    const node = nodes.get(id)!
    node.summary = user.login.toLowerCase() === selectedLogin ? `${text('createKey')}: ${text(user.can_create_key === true ? 'allowed' : user.can_create_key === false ? 'denied' : 'unknown')}` : text('user')
    node.details.push([text('name'), user.name || user.login], [text('accountType'), user.kind], [text('createKey'), node.summary])
    if (snapshot.memberScope && nodes.has(snapshot.memberScope)) connect(snapshot.memberScope, id, 'structure', text('member'))
  }
  for (const login of auth.admin_logins ?? []) if (login.toLowerCase() === selectedLogin) connect(identity('user', login), admins, 'access', text('administrator'))
  if (selectedLogin && auth.local_admin?.enabled && auth.local_admin.username?.toLowerCase() === selectedLogin) connect(identity('user', selectedLogin), admins, 'access', text('localAdmin'))

  const policy = (id: string, label: string, kind: string, summary: string, details: [string, string][], href: string, inactive = false) =>
    add({ id, label, kind, layer: 'policy', summary, details, href, inactive })
  const login = policy('login', text('login'), 'access', auth.allow_any_github_user === false ? text('adminsOnly') : text('allGithub'), [], '/access/admins')
  connect(auth.allow_any_github_user === false ? admins : everyone, login, 'access', text('allowed'))
  const bypass = policy('bypass', text('adminBypass'), 'access', text('unrestricted'), [[text('semantics'), text('bypassDetail')]], '/access/admins')
  connect(admins, bypass, 'access', text('administrator'))
  const create = policy('create', text('createKey'), 'access', keyPolicy.enabled ? 'OR' : text('open'), [[text('status'), status(keyPolicy.enabled)], [text('semantics'), text('createDetail')]], '/access/policy')
  if (!keyPolicy.enabled) connect(everyone, create, 'access', text('open'))
  for (const [slug, rule] of Object.entries(keyPolicy.enterprises ?? {})) {
    const parent = identity('enterprise', slug)
    const inactive = !keyPolicy.enabled || !rule.enabled
    nodes.get(parent)!.details.push([text('createKey'), status(rule.enabled)], [text('allOrgs'), String(Boolean(rule.allow_all_orgs))])
    if (rule.allow_all_orgs) connect(parent, create, 'access', text('allOrgs'), inactive)
    for (const org of rule.organizations ?? []) {
      const id = identity('organization', org)
      connect(parent, id, 'structure', text('configuredScope'))
      connect(id, create, 'access', 'OR', inactive)
    }
    for (const team of rule.teams ?? []) {
      const id = identity('team', `${slug}/${team}`)
      connect(parent, id, 'structure', text('configuredScope'))
      connect(id, create, 'access', 'OR', inactive)
    }
  }
  const levels = [scopePolicy.users, scopePolicy.teams, scopePolicy.organizations].filter(level => level?.length).length
  const scope = policy('scope', text('scopePermission'), 'scope', scopePolicy.enabled && levels ? 'AND' : text('grantsNobody'), [[text('status'), status(scopePolicy.enabled)], [text('semantics'), text('scopeDetail')]], '/access/keyscope', !scopePolicy.enabled)
  const scopeLevels = [['user', scopePolicy.users], ['team', scopePolicy.teams], ['organization', scopePolicy.organizations]] as const
  for (const [kind, values] of scopeLevels) for (const value of values ?? []) if (kind !== 'user' || value.toLowerCase() === selectedLogin) connect(identity(kind, value), scope, 'scope', `${text(kind)}: OR`, !scopePolicy.enabled)

  for (const [name, meta] of Object.entries(config.models)) {
    const provider = meta.provider || config.default_provider
    add({ id: `model:${name}`, label: name, kind: 'model', layer: 'model', summary: `${provider} / ${config.providers[provider]?.api_type || 'azure'}`, href: '/config/models', details: [[text('description'), meta.description || text('none')], [text('provider'), provider], [text('deployment'), meta.model_name || name], [text('api'), meta.api || 'chat'], [text('default'), String(Boolean(meta.default))]] })
    connect(bypass, `model:${name}`, 'models', text('unrestricted'))
  }
  const groups = config.model_groups ?? {}
  for (const [name, models] of Object.entries(groups)) {
    const group = policy(`group:${name}`, name, 'group', `${text('union')} / ${models.length} ${text('models')}`, [[text('status'), status(modelPolicy.enabled)], [text('semantics'), text(models.length ? 'groupDetail' : 'emptyGroup')], [text('models'), models.join(', ') || text('none')]], '/policy', !modelPolicy.enabled)
    for (const model of models) if (config.models[model]) connect(group, `model:${model}`, 'models', text('union'), !modelPolicy.enabled)
  }
  for (const [kind, bindings] of [['user', modelPolicy.users], ['team', modelPolicy.teams], ['organization', modelPolicy.organizations]] as const) {
    for (const [name, group] of Object.entries(bindings ?? {})) {
      if (kind === 'user' && name.toLowerCase() !== selectedLogin) continue
      const source = identity(kind, name)
      if (nodes.has(`group:${group}`)) connect(source, `group:${group}`, 'models', text('binding'), !modelPolicy.enabled)
    }
  }
  if (modelPolicy.default_group && nodes.has(`group:${modelPolicy.default_group}`)) connect(everyone, `group:${modelPolicy.default_group}`, 'models', text('defaultGroup'), !modelPolicy.enabled)
  const unrestricted = policy('unrestricted', text('modelFallback'), 'group', modelPolicy.enabled ? text('noBinding') : text('policyOff'), [[text('semantics'), text('fallbackDetail')]], '/policy')
  connect(everyone, unrestricted, 'models', modelPolicy.enabled ? text('noBinding') : text('policyOff'))
  for (const model of Object.keys(config.models)) connect(unrestricted, `model:${model}`, 'models', text('unrestricted'))

  for (const key of keys) {
    if (key.user_login.toLowerCase() !== selectedLogin) continue
    const scope = key.scope ?? { kind: 'all' }
    const selected = Object.entries(config.models).filter(([name, meta]) => scope.kind === 'all' || (scope.kind === 'models' ? scope.models.includes(name) : scope.api_types.includes(config.providers[meta.provider || config.default_provider]?.api_type ?? 'azure'))).map(([name]) => name)
    const keyNode = policy(`key:${key.id}`, key.name || key.id, 'key', `${scope.kind} / ${status(!key.disabled)}`, [[text('owner'), key.user_login], [text('identifier'), key.id], [text('status'), status(!key.disabled)], [text('scope'), scope.kind === 'all' ? text('allOwnerModels') : (scope.kind === 'models' ? scope.models : scope.api_types).join(', ')], [text('semantics'), text('keyDetail')]], '/keys', key.disabled)
    connect(identity('user', key.user_login), keyNode, 'keys', text('owner'), key.disabled)
    for (const model of selected) connect(keyNode, `model:${model}`, 'keys', text('scopeCandidate'), key.disabled)
  }
  if (snapshot.userPolicy && selectedLogin) {
    const result = snapshot.userPolicy
    const user = identity('user', selectedLogin)
    nodes.get(user)!.summary = `${text('createKey')}: ${text(result.access ? result.access.allowed ? 'allowed' : 'denied' : 'unknown')}`
    nodes.get(user)!.details.push([text('createKey'), result.access?.reason ?? text('unknown')], [text('scopePermission'), result.key_scope?.reason ?? text('unknown')])
    const effective = policy('effective', text('effectiveModels'), 'group', result.model_policy ? `${result.model_policy.models.length} ${text('models')}` : text('unknown'), [[text('status'), result.model_policy?.reason ?? text('unknown')]], '/policy')
    connect(user, effective, 'models', text('effectiveModels'))
    if (result.access?.allowed) connect(user, create, 'access', text('allowed'))
    if (result.key_scope?.allowed) connect(user, scope, 'scope', text('allowed'))
    for (const contribution of result.model_policy?.contributions ?? []) {
      if (nodes.has(`group:${contribution.group}`)) connect(user, `group:${contribution.group}`, 'models', `${text(contribution.scope === 'default' ? 'defaultGroup' : contribution.scope)}: ${contribution.name}`)
    }
    for (const name of result.model_policy?.models ?? []) if (nodes.has(`model:${name}`)) connect(effective, `model:${name}`, 'models', text('effectiveModels'))
  }
  return { nodes: [...nodes.values()], edges }
}

