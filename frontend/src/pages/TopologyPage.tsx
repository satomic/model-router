import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Link } from 'react-router-dom'
import { Background, Handle, MarkerType, Position, ReactFlow, ReactFlowProvider, useReactFlow, type NodeProps, type Node } from '@xyflow/react'
import { ArrowLeft, ArrowRight, ArrowUpRight, Building2, ChevronLeft, ChevronRight, CircleUserRound, Focus, GitBranch, Info, KeyRound, Layers, Network, RefreshCw, Search, ShieldCheck, Users, X, ZoomIn, ZoomOut } from 'lucide-react'
import { discoverEnterprises, getCacheStatus, getConfig, getTopologyMembers, getTopologyUser, type TopologyMembers } from '../api'
import { formatDateTime } from '../i18n/format'
import { buildGraph, neighborhoodPositions, policyNeighborhood, type Category, type GraphNode, type Snapshot } from './topology/graph'
import { dataIssues, type DataIssue } from './topology/status'
import '@xyflow/react/dist/style.css'
import './topology/topology.css'

const ICONS = { enterprise: Building2, organization: Building2, team: Users, user: CircleUserRound, everyone: Users, admins: ShieldCheck, access: ShieldCheck, scope: GitBranch, group: Layers, key: KeyRound, model: Network }
type TopologyNode = Node<GraphNode & { faded: boolean }, 'topology'>

function TopologyCard({ data, selected }: NodeProps<TopologyNode>) {
  const Icon = ICONS[data.kind as keyof typeof ICONS] ?? Layers
  return (
    <div className={`topology-node tone-${data.kind} ${data.faded ? 'faded' : ''} ${data.inactive ? 'inactive' : ''} ${selected ? 'chosen' : ''}`}>
      <Handle type="target" position={Position.Top} isConnectable={false} />
      <div className="topology-node-title"><Icon size={18} /><strong title={data.label}>{data.label}</strong></div>
      <div className="topology-node-summary" title={data.summary}>{data.summary}</div>
      <Handle type="source" position={Position.Bottom} isConnectable={false} />
    </div>
  )
}
const nodeTypes = { topology: TopologyCard }
const categories: Category[] = ['access', 'scope', 'models', 'keys']

function TopologyView() {
  const { t } = useTranslation()
  const text = (key: string) => t(`topology.${key}`)
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [warnings, setWarnings] = useState<DataIssue[]>([])
  const [updated, setUpdated] = useState<Date | null>(null)
  const [query, setQuery] = useState('')
  const [category, setCategory] = useState<Category | 'all'>('all')
  const [selection, setSelection] = useState<{ kind: 'node' | 'edge'; id: string } | null>(null)
  const [trail, setTrail] = useState<string[]>([])
  const [hovered, setHovered] = useState<string | null>(null)
  const [relationPage, setRelationPage] = useState(0)
  const [tab, setTab] = useState<'policies' | 'identities' | 'models'>('policies')
  const [compact, setCompact] = useState(() => window.innerWidth < 700)
  const [memberTarget, setMemberTarget] = useState<{ kind: string; name: string; id: string; label: string } | null>(null)
  const [members, setMembers] = useState<TopologyMembers | null>(null)
  const [memberLoading, setMemberLoading] = useState(false)
  const [memberError, setMemberError] = useState('')
  const [userLoading, setUserLoading] = useState('')
  const memberRequest = useRef<AbortController | null>(null)
  const userRequest = useRef<AbortController | null>(null)
  const canvas = useRef<HTMLDivElement>(null)
  const inspector = useRef<HTMLElement>(null)
  const { fitView, setViewport, zoomIn, zoomOut } = useReactFlow()

  async function load() {
    memberRequest.current?.abort()
    userRequest.current?.abort()
    setMemberTarget(null)
    setMembers(null)
    setUserLoading('')
    setMemberLoading(false)
    setMemberError('')
    setSelection(null)
    setTrail([])
    setRelationPage(0)
    setLoading(true)
    setError('')
    try {
      const config = await getConfig()
      setSnapshot({ config, enterprises: [], users: null, keys: [] })
      setWarnings([])
      const results = await Promise.allSettled([discoverEnterprises(false, AbortSignal.timeout(15000)), getCacheStatus()] as const)
      const [discovery, cache] = results
      const missing = dataIssues(discovery.status === 'fulfilled' ? discovery.value.enterprises : [], cache.status === 'fulfilled' ? cache.value : null, discovery.status === 'fulfilled' ? discovery.value.fetched_at : undefined)
      results.forEach((result, index) => {
        if (result.status === 'rejected') missing.push({ scope: index === 0 ? '/v1/access/discover' : '/v1/access/cache', key: 'statusRequestError', reason: String(result.reason) })
      })
      setSnapshot(previous => previous && { ...previous, enterprises: discovery.status === 'fulfilled' ? discovery.value.enterprises : [] })
      setWarnings(missing)
      setUpdated(new Date())
    } catch (reason) {
      setError(String(reason))
    } finally {
      setLoading(false)
    }
  }

  function clearUser() {
    userRequest.current?.abort()
    setUserLoading('')
    setSelection(null)
    setTrail(previous => previous.filter(id => !id.startsWith('user:') && !id.startsWith('key:') && id !== 'effective'))
    setSnapshot(previous => previous && { ...previous, users: null, keys: [], selectedLogin: undefined, memberScope: undefined, userPolicy: undefined })
  }

  async function loadMembers(target: NonNullable<typeof memberTarget>, page = 1) {
    memberRequest.current?.abort()
    clearUser()
    const controller = new AbortController()
    memberRequest.current = controller
    setMemberTarget(target)
    setMembers(null)
    setMemberLoading(true)
    setMemberError('')
    setQuery('')
    try {
      const result = await getTopologyMembers(target.kind, target.name, page, controller.signal)
      if (!controller.signal.aborted) setMembers(result)
    } catch (reason) {
      if (!controller.signal.aborted) setMemberError(String(reason))
    } finally {
      if (!controller.signal.aborted) setMemberLoading(false)
    }
  }

  async function selectUser(login: string) {
    clearUser()
    const controller = new AbortController()
    userRequest.current = controller
    setUserLoading(login)
    setMemberError('')
    try {
      const result = await getTopologyUser(login, controller.signal)
      if (controller.signal.aborted) return
      setSnapshot(previous => previous && { ...previous, selectedLogin: login, memberScope: memberTarget?.kind === 'known' ? undefined : memberTarget?.id, users: { users: [{ login, name: login, kind: 'github', first_seen: 0, last_seen: 0, sign_ins: 0, model_group: '', can_create_key: result.access?.allowed ?? null }], default_group: '', policy_enabled: true }, keys: result.keys, userPolicy: result })
      setSelection(null)
      setTrail([`user:${login.toLowerCase()}`, 'effective'])
      setRelationPage(0)
      setCategory('all')
      setQuery('')
    } catch (reason) {
      if (!controller.signal.aborted) setMemberError(String(reason))
    } finally {
      if (!controller.signal.aborted) setUserLoading('')
    }
  }

  function selectNode(node: GraphNode) {
    if (trail[trail.length - 1] === node.id) {
      setSelection({ kind: 'node', id: node.id })
      return
    }
    setSelection(null)
    setTrail(previous => [...previous, node.id])
    setRelationPage(0)
    setHovered(null)
    if (node.kind === 'organization' || node.kind === 'team') void loadMembers({ kind: node.kind, name: node.id.slice(node.id.indexOf(':') + 1), id: node.id, label: node.label })
    if (node.kind === 'user' && node.id !== `user:${snapshot?.selectedLogin}`) void selectUser(node.label)
  }

  function overview() {
    memberRequest.current?.abort()
    clearUser()
    setTrail([])
    setMemberTarget(null)
    setMembers(null)
    setQuery('')
    setRelationPage(0)
  }

  useEffect(() => { void load(); return () => { memberRequest.current?.abort(); userRequest.current?.abort() } }, [])
  useEffect(() => { setRelationPage(0) }, [compact])
  useEffect(() => {
    if (!selection) return
    const frame = requestAnimationFrame(() => inspector.current?.scrollIntoView({ block: 'nearest' }))
    return () => cancelAnimationFrame(frame)
  }, [selection])
  useEffect(() => {
    const escape = (event: KeyboardEvent) => { if (event.key === 'Escape') setSelection(null) }
    document.addEventListener('keydown', escape)
    return () => document.removeEventListener('keydown', escape)
  }, [])

  const graph = snapshot ? buildGraph(snapshot, text) : { nodes: [], edges: [] }
  const needle = query.trim().toLowerCase()
  const centerId = trail[trail.length - 1]
  const center = graph.nodes.find(node => node.id === centerId)
  const neighborhood = center ? policyNeighborhood(graph, center.id) : { nodes: [], edges: [] }
  const neighbors = neighborhood.nodes.filter(node => node.id !== centerId && (!needle || `${node.label} ${node.summary} ${node.details.flat().join(' ')}`.toLowerCase().includes(needle)))
  const incomingIds = new Set(neighborhood.edges.filter(edge => edge.target === centerId).map(edge => edge.source))
  const upstream = neighbors.filter(node => incomingIds.has(node.id))
  const downstream = neighbors.filter(node => !incomingIds.has(node.id))
  const pageSize = compact ? 1 : 3
  const pageCount = Math.max(1, Math.ceil(upstream.length / pageSize), Math.ceil(downstream.length / pageSize))
  const pageIndex = Math.min(relationPage, pageCount - 1)
  const pageSide = (items: GraphNode[]) => {
    const start = Math.min(pageIndex, Math.max(0, Math.ceil(items.length / pageSize) - 1)) * pageSize
    return items.slice(start, start + pageSize)
  }
  const visible = center ? [center, ...pageSide(upstream), ...pageSide(downstream)] : []
  const visibleIds = new Set(visible.map(node => node.id))
  const filteredEdges = neighborhood.edges.filter(edge => visibleIds.has(edge.source) && visibleIds.has(edge.target))
  const selectedNode = graph.nodes.find(node => selection?.kind === 'node' && node.id === selection.id)
  const selectedEdge = selection?.kind === 'edge' ? neighborhood.edges.find(edge => edge.id === selection.id) ?? graph.edges.find(edge => edge.id === selection.id) : undefined
  const positions = neighborhoodPositions({ nodes: visible, edges: filteredEdges }, centerId)
  const nodes: TopologyNode[] = visible.map(node => {
    return { id: node.id, type: 'topology', data: { ...node, faded: false }, position: positions.get(node.id)!, width: 220, height: 82, measured: { width: 220, height: 82 }, handles: [{ type: 'target', position: Position.Top, x: 107, y: -3, width: 6, height: 6 }, { type: 'source', position: Position.Bottom, x: 107, y: 79, width: 6, height: 6 }], selected: node.id === centerId, ariaLabel: `${node.label}, ${node.summary}`, draggable: false }
  })
  const edges = filteredEdges.map(edge => {
    const active = hovered === edge.id || hovered === edge.source || hovered === edge.target || selectedEdge?.id === edge.id
    return { ...edge, type: 'default', label: active ? edge.label : undefined, className: `topology-edge edge-${edge.kind}`, style: { strokeWidth: active ? 2.5 : 1.5, strokeDasharray: edge.inactive ? '5 5' : undefined, opacity: hovered && !active ? 0.18 : edge.inactive ? 0.45 : 0.65 }, markerEnd: { type: MarkerType.ArrowClosed, color: 'var(--line-strong)' }, labelStyle: { fill: 'var(--text)', fontSize: 11 }, labelBgStyle: { fill: 'var(--bg-panel)' }, interactionWidth: 20, ariaLabel: `${edge.label}: ${graph.nodes.find(node => node.id === edge.source)?.label} → ${graph.nodes.find(node => node.id === edge.target)?.label}` }
  })
  const layoutKey = [...positions].map(([id, position]) => `${id}:${position.x}:${position.y}`).join('|')
  useEffect(() => {
    if (!center || !canvas.current) return
    const reset = () => {
      if (!canvas.current) return
      setCompact(canvas.current.clientWidth < 560)
      const width = Math.max(...[...positions.values()].map(position => position.x + 220), 220)
      const height = Math.max(...[...positions.values()].map(position => position.y + 82), 82)
      const zoom = Math.min(1, Math.max(0.45, (canvas.current.clientWidth - 64) / width), Math.max(0.6, (canvas.current.clientHeight - 64) / height))
      void setViewport({ x: Math.max(24, (canvas.current.clientWidth - width * zoom) / 2), y: 32, zoom })
    }
    const observer = new ResizeObserver(reset)
    observer.observe(canvas.current)
    reset()
    return () => observer.disconnect()
  }, [centerId, layoutKey, setViewport])

  const matching = graph.nodes.filter(node => {
    const layer = tab === 'policies' ? 'policy' : tab === 'identities' ? 'identity' : 'model'
    const kind = ({ access: 'access', scope: 'scope', models: 'group', keys: 'key' } as const)[category as Category]
    return node.layer === layer && (category === 'all' || tab !== 'policies' || node.kind === kind) && (!needle || `${node.label} ${node.summary} ${node.details.flat().join(' ')}`.toLowerCase().includes(needle))
  })
  const indexPages = Math.max(1, Math.ceil(matching.length / 24))
  const indexPage = Math.min(relationPage, indexPages - 1)

  const details = selectedNode?.details ?? (selectedEdge ? [[text('relationship'), selectedEdge.label], [text('source'), graph.nodes.find(node => node.id === selectedEdge.source)?.label ?? ''], [text('target'), graph.nodes.find(node => node.id === selectedEdge.target)?.label ?? ''], [text('status'), text(selectedEdge.inactive ? 'disabled' : 'configured')]] : [])
  const href = selectedNode?.href ?? (selectedEdge ? graph.nodes.find(node => node.id === selectedEdge.target)?.href : undefined)
  return (
    <section className="topology-page" aria-label={text('title')}>
      <div className="topology-toolbar">
        {center && <button className="icon-btn" title={text('back')} aria-label={text('back')} onClick={() => { if (trail.length <= 1) overview(); else setTrail(previous => previous.slice(0, -1)); setSelection(null); setQuery(''); setRelationPage(0) }}><ArrowLeft size={18} /></button>}
        <label className="topology-search"><Search size={16} /><input aria-label={text('search')} placeholder={text('search')} value={query} onChange={event => { setQuery(event.target.value); setRelationPage(0); setSelection(null) }} />{query && <button className="icon-btn" title={text('clear')} aria-label={text('clear')} onClick={() => setQuery('')}><X size={15} /></button>}</label>
        {!center && <>
        <div className="topology-tabs" role="tablist" aria-label={text('overview')}>
          {(['policies', 'identities', 'models'] as const).map(value => <button role="tab" aria-selected={tab === value} key={value} onClick={() => { setTab(value); setQuery(''); setRelationPage(0) }}>{text(value)}</button>)}
        </div>
        {tab === 'policies' &&
        <select aria-label={text('filter')} value={category} onChange={event => { setCategory(event.target.value as Category | 'all'); setRelationPage(0); setSelection(null) }}>
          <option value="all">{text('allPolicies')}</option>
          {categories.map(value => <option key={value} value={value}>{text(value === 'models' ? 'modelPolicy' : value)}</option>)}
        </select>
        }</>}
        <button className="btn ghost" disabled={loading} onClick={() => void loadMembers({ kind: 'known', name: '', id: 'known', label: text('knownUsers') })}><Users size={15} />{text('knownUsers')}</button>
        {(center || snapshot?.selectedLogin) && <button className="btn ghost" onClick={overview}><Layers size={15} />{text('overview')}</button>}
        <span className="spacer" />
        <button className="btn ghost" disabled={loading} onClick={() => void load()}><RefreshCw size={15} className={loading ? 'topology-spinning' : ''} />{t('common.reload')}</button>
      </div>
      <div className="topology-meta"><span>{text('savedSnapshot')}{updated ? ` · ${formatDateTime(updated.toISOString())}` : ''}</span><span>{center ? `${neighbors.length} ${text('connections')}` : `${matching.length} ${text(tab)}`}</span></div>
      {error && <div className="toast error" role="alert">{error}</div>}
      {warnings.length > 0 && <details className="topology-warning"><summary>{text('dataWarnings')} ({warnings.length})</summary><ul className="topology-issues" role="status">{warnings.map((issue, index) => <li key={`${issue.scope}:${issue.key}:${index}`}>
        <strong>{issue.scope}</strong>
        <p>{t(`topology.${issue.key}`, issue.values ?? {})}</p>
        {issue.reason && <p className="topology-issue-reason"><span>{text('statusReason')}: </span>{issue.reason}</p>}
        {issue.fetchedAt !== undefined && <small>{text('statusFetched')}: {issue.fetchedAt > 0 ? formatDateTime(new Date(issue.fetchedAt * 1000).toISOString()) : text('unknown')}</small>}
      </li>)}</ul></details>}
      <div className="topology-workspace">
        {memberTarget && <aside className="topology-members" aria-label={text('members')}>
          <div className="topology-detail-heading"><strong>{memberTarget.label}</strong><button className="icon-btn" aria-label={text('closeMembers')} title={text('closeMembers')} onClick={() => { memberRequest.current?.abort(); clearUser(); setTrail(previous => previous.filter(id => !id.startsWith('user:') && !id.startsWith('key:') && id !== 'effective')); setMemberTarget(null); setMembers(null) }}><X size={16} /></button></div>
          {memberLoading && <p role="status">{text('loading')}</p>}
          {memberError && <div role="alert">{memberError}<button className="btn ghost" onClick={() => void loadMembers(memberTarget, members?.page ?? 1)}><RefreshCw size={14} />{t('common.reload')}</button></div>}
          {members && <><div className="topology-member-list">{members.users.map(user => <button key={user.login} className={snapshot?.selectedLogin === user.login ? 'selected' : ''} onClick={() => void selectUser(user.login)}><CircleUserRound size={16} /><span>{user.login}</span>{userLoading === user.login && <RefreshCw size={14} className="topology-spinning" />}</button>)}</div>{!members.users.length && <p>{text('noMembers')}</p>}<div className="topology-member-pages"><button className="icon-btn" title={text('previous')} aria-label={text('previous')} disabled={members.page <= 1} onClick={() => void loadMembers(memberTarget, members.page - 1)}><ChevronLeft size={16} /></button><span>{members.page}</span><button className="icon-btn" title={text('next')} aria-label={text('next')} disabled={!members.has_more} onClick={() => void loadMembers(memberTarget, members.page + 1)}><ChevronRight size={16} /></button></div></>}
        </aside>}
        <div className="topology-canvas-wrap">
          {!center ? <div className="topology-index">
            {matching.slice(indexPage * 24, (indexPage + 1) * 24).map(node => {
              const Icon = ICONS[node.kind as keyof typeof ICONS] ?? Layers
              const incoming = new Set(graph.edges.filter(edge => edge.target === node.id).map(edge => edge.source)).size
              const outgoing = new Set(graph.edges.filter(edge => edge.source === node.id).map(edge => edge.target)).size
              return <button key={node.id} className={`topology-index-item tone-${node.kind} ${node.inactive ? 'inactive' : ''}`} onClick={() => { setQuery(''); selectNode(node) }}>
                <div className="topology-index-title"><span className="topology-index-icon"><Icon size={22} /></span><strong>{node.label}</strong><ArrowRight size={16} /></div>
                <p>{node.summary}</p>
                <div className="topology-index-counts"><span><span className="count-dot incoming" />{incoming} {text('upstream')}</span><span><span className="count-dot outgoing" />{outgoing} {text('downstream')}</span></div>
              </button>
            })}
            {!matching.length && <div className="empty">{text(loading ? 'loading' : 'noResults')}</div>}
          </div> : <>
          <div className="topology-trail">{trail.slice(Math.max(0, trail.length - 3), -1).map((id, index) => <span className="topology-history" key={`${id}:${index}`}><button onClick={() => { setTrail(previous => previous.slice(0, Math.max(0, trail.length - 3) + index + 1)); setSelection(null); setQuery(''); setRelationPage(0) }}>{graph.nodes.find(node => node.id === id)?.label ?? id}</button><ChevronRight size={14} /></span>)}<strong>{center.label}</strong><span className="dim">{center.summary}</span><span className="spacer" /><button className="icon-btn" title={text('details')} aria-label={text('details')} onClick={() => setSelection({ kind: 'node', id: center.id })}><Info size={17} /></button></div>
          <div className="topology-canvas" ref={canvas}>
            <ReactFlow nodes={nodes} edges={edges} nodeTypes={nodeTypes} nodesConnectable={false} nodesDraggable={false} minZoom={0.05} maxZoom={1.6} panOnScroll zoomOnScroll={false} onNodeClick={(_event, node) => { setQuery(''); selectNode(node.data) }} onNodeMouseEnter={(_event, node) => setHovered(node.id === centerId ? null : node.id)} onNodeMouseLeave={() => setHovered(null)} onEdgeMouseEnter={(_event, edge) => setHovered(edge.id)} onEdgeMouseLeave={() => setHovered(null)} onEdgeClick={(_event, edge) => setSelection({ kind: 'edge', id: edge.id })} onPaneClick={() => setSelection(null)}>
              <Background color="var(--line-hi)" gap={20} size={1} />
            </ReactFlow>
            {!neighbors.length && <div className="topology-empty" role="status">{text(needle ? 'noResults' : 'noConnections')}</div>}
            <div className="topology-controls">
              <button title={text('zoomIn')} aria-label={text('zoomIn')} onClick={() => void zoomIn()}><ZoomIn size={18} /></button>
              <button title={text('zoomOut')} aria-label={text('zoomOut')} onClick={() => void zoomOut()}><ZoomOut size={18} /></button>
              <button title={text('fit')} aria-label={text('fit')} onClick={() => void fitView({ padding: 0.15 })}><Focus size={18} /></button>
            </div>
          </div></>}
          {(center ? pageCount : indexPages) > 1 && <div className="topology-pagination"><button className="icon-btn" title={text('previous')} aria-label={text('previous')} disabled={(center ? pageIndex : indexPage) === 0} onClick={() => setRelationPage(previous => previous - 1)}><ChevronLeft size={18} /></button><span>{(center ? pageIndex : indexPage) + 1} / {center ? pageCount : indexPages}</span><button className="icon-btn" title={text('next')} aria-label={text('next')} disabled={(center ? pageIndex : indexPage) + 1 === (center ? pageCount : indexPages)} onClick={() => setRelationPage(previous => previous + 1)}><ChevronRight size={18} /></button></div>}
        </div>
        {selection && <aside className="topology-detail" aria-label={text('details')} ref={inspector}>
          <div className="topology-detail-heading"><span>{text('details')}</span><button className="icon-btn" aria-label={t('common.close')} title={t('common.close')} onClick={() => setSelection(null)}><X size={18} /></button></div>
          <h2>{selectedNode?.label ?? selectedEdge?.label}</h2>
          <p className="dim">{selectedNode?.summary}</p>
          <dl>{details.map(([label, value], index) => <div key={index}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>
          {selectedEdge && <div className="topology-endpoints">{[selectedEdge.source, selectedEdge.target].map(id => <button key={id} className="btn ghost" onClick={() => { const node = graph.nodes.find(node => node.id === id); if (node) selectNode(node) }}>{graph.nodes.find(node => node.id === id)?.label}</button>)}</div>}
          {selectedNode && <div className="topology-connections"><h3>{text('connections')}</h3>{graph.edges.filter(edge => edge.source === selectedNode.id || edge.target === selectedNode.id).map(edge => <button key={edge.id} onClick={() => setSelection({ kind: 'edge', id: edge.id })}><span>{graph.nodes.find(node => node.id === (edge.source === selectedNode.id ? edge.target : edge.source))?.label}</span><small>{edge.label}{edge.inactive ? ` · ${text('disabled')}` : ''}</small></button>)}</div>}
          {href && <Link className="btn ghost topology-config-link" to={href}>{text('openConfig')}<ArrowUpRight size={16} /></Link>}
        </aside>}
      </div>
      <div className="topology-legend">{['enterprise', 'organization', 'team', 'user', 'policies', 'models'].map(kind => <span key={kind}><i className={`node-legend-${kind}`} />{text(kind)}</span>)}<span><i className="legend-inactive" />{text('disabled')}</span></div>
    </section>
  )
}

export default function TopologyPage() {
  return <ReactFlowProvider><TopologyView /></ReactFlowProvider>
}