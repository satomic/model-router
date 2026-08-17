/** Backend API client and type definitions. */

export interface TraceSummary {
  id: string
  ts: string
  user_id: string | null
  session_id: string | null
  /** Ties the turns of one user interaction together. An agentic client sends this on the
   *  original question and on every follow-up of its tool-call loop, so all of them land in
   *  one record. Null for a client that sends no such header. */
  interaction_id: string | null
  /** How many upstream calls that one interaction took. 1 is the ordinary case; more means a
   *  tool-call loop. */
  turn_count: number
  strategy: string
  model: string
  reason: string
  decision_ms: number
  total_ms: number | null
  status: string
  stream: boolean
  prompt_preview: string
}

export interface RuleStep {
  rule: string
  model: string
  matched: boolean
  check?: string
  matched_keyword?: string
  skipped?: string
}

export interface RoutingAnalysis {
  type: 'rule' | 'ai' | 'session'
  evaluated?: RuleStep[]
  fallback?: string | boolean
  note?: string
  decision_model?: string
  decision_provider?: string
  decision_input?: string
  /** The system prompt actually sent for this request -- the prompt is configurable, so it is
   *  recorded per trace. */
  decision_system?: string
  prompt_truncated?: boolean
  candidates?: string[]
  raw_response?: string
  rationale?: string
  decision_latency_ms?: number
  decision_usage?: { prompt_tokens: number; completion_tokens: number }
  error?: string
  session_bound?: string
  interaction_bound?: string
  /** Which key answered a skipped decision: the interaction (one user question) or the
   *  broader session. */
  bound_by?: 'interaction' | 'session'
}

/** A tool call the model asked for. Only the fields the console renders are typed; the rest of
 *  the upstream shape is passed through to the JSON viewer. */
export interface ToolCall {
  id?: string | null
  type?: string
  function?: { name?: string | null; arguments?: string | null }
}

export interface TraceResponse {
  content: string | null
  finish_reason: string | null
  usage: Record<string, unknown> | null
  tool_calls?: ToolCall[] | null
}

/** One upstream call within an interaction. The first turn is the user's question; each later
 *  one is the agent replaying the conversation with a tool result appended. */
export interface TraceTurn {
  index: number
  ts: string
  request_id?: string | null
  /** Copilot's x-initiator: "user" for the request the user triggered, "agent" for the tool
   *  loop's follow-ups. */
  initiator?: string | null
  message_count: number
  /** Present only when this turn's chain cannot be read off the record's top-level messages --
   *  i.e. the client rewrote history rather than appending to it. */
  messages?: unknown[]
  rewritten?: boolean
  superseded?: boolean
  params?: Record<string, unknown>
  stream?: boolean
  model?: string
  deployment?: string
  status: string
  total_ms: number | null
  response: TraceResponse | null
  error?: string | null
}

export interface TraceDetail {
  id: string
  ts: string
  user_id: string | null
  api_key_id?: string
  api_key_name?: string
  session_id: string | null
  interaction_id: string | null
  request_id?: string | null
  initiator?: string | null
  strategy: string
  sticky: boolean
  prompt_preview: string
  /** Every upstream call this interaction took, oldest first. */
  turns: TraceTurn[]
  turn_count: number
  /** How many turns were dropped by the per-record cap, when it was reached. */
  turns_truncated?: number
  /** `messages` is the conversation as it stood on the final turn, so it contains every tool
   *  call and tool result of the whole interaction. */
  request: { messages: unknown[]; params: Record<string, unknown>; stream: boolean }
  routing: { model: string; reason: string; decision_ms: number; analysis: RoutingAnalysis }
  backend: {
    deployment: string
    api: string
    provider?: string
    base_url?: string
    api_type?: string
    sent_params: Record<string, unknown>
    latency_ms?: number
  }
  response: TraceResponse | null
  /** The interaction's total token usage, summed over its turns. Outside `response` so it
   *  survives an interaction whose last turn failed. */
  usage?: Record<string, unknown> | null
  status: string
  total_ms: number | null
  error?: string
}

export interface ModelMeta {
  description?: string
  default?: boolean
  reasoning?: boolean
  api?: string
  provider?: string
  model_name?: string
}

export interface ProviderMeta {
  base_url?: string
  api_key?: string
  api_type?: 'azure' | 'openai'
  api_version?: string
}

export interface Rule {
  name: string
  keywords?: string[]
  min_prompt_chars?: number
  model: string
}

/** The key policy for one enterprise: a master switch plus second-level
 *  organization / enterprise-team rules. */
export interface EnterpriseRule {
  enabled?: boolean
  /** true = membership of any organization in the enterprise suffices (no need to tick
   *  organizations one by one). */
  allow_all_orgs?: boolean
  organizations?: string[]
  /** The **numeric id** of the enterprise team -- GitHub's membership endpoint does not accept
   *  the ent:-prefixed slug. */
  teams?: (number | string)[]
}

export interface KeyPolicy {
  enabled?: boolean
  github_token?: string
  /** How often the local copy of the enterprise structure and member lists is refreshed. */
  cache_refresh_seconds?: number
  enterprises?: Record<string, EnterpriseRule>
}

/** The local super administrator. Note there is no plaintext password here and no hash either:
 *  the credential is written through /v1/auth/local/password and is never part of a config draft. */
export interface LocalAdminConfig {
  enabled?: boolean
  username?: string
  updated_at?: number | null
}

export interface AuthConfig {
  github: { client_id?: string; client_secret?: string; callback_url?: string }
  admin_logins?: string[]
  allow_any_github_user?: boolean
  session_ttl_seconds?: number
  key_policy?: KeyPolicy
  local_admin?: LocalAdminConfig
}

export interface RouterConfig {
  strategy: 'rule' | 'ai'
  session: { sticky: boolean; ttl_seconds: number; max_sessions: number }
  ai_router: {
    decision_model: string
    decision_provider?: string
    timeout_seconds: number
    max_prompt_chars: number
    /** The system prompt for the AI decision, carrying the {catalog} placeholder. Empty = use
     *  the backend's built-in default. */
    decision_prompt?: string
  }
  providers: Record<string, ProviderMeta>
  default_provider: string
  models: Record<string, ModelMeta>
  rules: Rule[]
  auth?: AuthConfig
}

export interface SessionUser {
  login: string
  name?: string
  avatar_url?: string | null
  is_admin: boolean
  /** true when signed in through the local super-administrator account rather than GitHub. */
  local_admin?: boolean
  /** true while the built-in default password is still in force. The server refuses every
   *  endpoint except status / logout / change-password until it is changed, so the console
   *  mirrors that by showing nothing but the change-password form. */
  must_change_password?: boolean
}

export interface AuthStatus {
  configured: boolean
  authenticated: boolean
  user: SessionUser | null
  can_setup: boolean
  callback_url: string
  /** true when the local username/password administrator is available -- the way in when
   *  github.com cannot be reached. */
  local_admin_enabled: boolean
  local_admin_username: string
}

export interface ApiKey {
  id: string
  name: string
  user_login: string
  prefix: string
  created_at: number
  last_used_at: number | null
  request_count: number
  disabled: boolean
  /** The plaintext key. Present only for the key's **owner** -- the administrator's ?all=1
   *  listing never carries it -- and absent on keys created before they became viewable, of
   *  which only the hash was ever stored. */
  key?: string
}

export interface UsageReport {
  scope: string
  is_admin: boolean
  days: number
  totals: {
    requests: number
    errors: number
    prompt_tokens: number
    completion_tokens: number
    total_tokens: number
    avg_ms: number | null
    p95_ms: number | null
  }
  by_model: { model: string; requests: number }[]
  by_day: { date: string; requests: number; total_tokens: number; errors: number }[]
  by_user: { user_id: string; requests: number; total_tokens: number }[]
}

/** The dedicated error thrown on a 401, so callers can switch back to the signed-out state. */
export class UnauthorizedError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'UnauthorizedError'
  }
}

async function ensureOk(res: Response): Promise<Response> {
  if (!res.ok) {
    let detail = res.statusText
    try {
      const body = await res.json()
      detail = typeof body.detail === 'string' ? body.detail : JSON.stringify(body.detail)
    } catch { /* ignore */ }
    if (res.status === 401) throw new UnauthorizedError(detail)
    throw new Error(`HTTP ${res.status}: ${detail}`)
  }
  return res
}

/** Always send the session cookie. */
function req(url: string, init: RequestInit = {}): Promise<Response> {
  return fetch(url, { credentials: 'include', ...init })
}

async function json<T>(url: string, init: RequestInit = {}): Promise<T> {
  return (await ensureOk(await req(url, init))).json() as Promise<T>
}

/** DELETE returning a JSON body (the delete endpoints report how much they removed). */
async function del<T>(url: string): Promise<T> {
  return json<T>(url, { method: 'DELETE' })
}

function jsonBody(method: string, body: unknown): RequestInit {
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }
}

// ── Version and releases ─────────────────────────────────────────
export interface Health {
  status: string
  strategy: string
  sticky: boolean
  providers: string[]
  version: string
  repo_url: string
  issues_url: string
  releases_url: string
}

export interface ReleaseStatus {
  current_version: string
  latest_version: string | null
  update_available: boolean
  release_url: string | null
  published_at: string | null
  checked_at: number | null
  error: string | null
}

export async function getHealth(): Promise<Health> {
  return json<Health>('/healthz')
}

/** The last answer from the background release check. Reads a cached result, so calling it on
 *  every page load costs nothing upstream. */
export async function getReleaseStatus(): Promise<ReleaseStatus> {
  return json<ReleaseStatus>('/v1/release')
}

/** Force a check now. Administrators only, since it makes an outbound request. */
export async function checkForUpdate(): Promise<ReleaseStatus> {
  return json<ReleaseStatus>('/v1/release/check', { method: 'POST' })
}

// ── Auth ─────────────────────────────────────────────────────────
export async function getAuthStatus(): Promise<AuthStatus> {
  return json<AuthStatus>('/v1/auth/status')
}

export async function setupAuth(payload: {
  client_id: string
  client_secret: string
  admin_logins: string[]
  callback_url?: string
}): Promise<{ ok: boolean; callback_url: string }> {
  return json('/v1/auth/setup', jsonBody('POST', payload))
}

export async function logout(): Promise<void> {
  await ensureOk(await req('/v1/auth/logout', { method: 'POST' }))
}

export const githubLoginUrl = '/v1/auth/github/login'

/** Sign in as the local super administrator. One 401 covers a wrong username and a wrong
 *  password alike -- distinguishing them would tell an attacker which half to keep guessing. */
export async function localLogin(username: string, password: string): Promise<{ ok: boolean; must_change_password: boolean }> {
  return json('/v1/auth/local/login', jsonBody('POST', { username, password }))
}

/** Change the local administrator's credential. Posted directly rather than through the Access
 *  page's shared auth draft: a password must not sit in a form draft that another tab's save
 *  could submit. */
export async function changeLocalPassword(payload: {
  current_password: string
  new_password: string
  new_username?: string
}): Promise<{ ok: boolean; username: string }> {
  return json('/v1/auth/local/password', jsonBody('POST', payload))
}

export async function setLocalAdminEnabled(enabled: boolean): Promise<{ ok: boolean; enabled: boolean }> {
  return json('/v1/auth/local/enabled', jsonBody('POST', { enabled }))
}

/** sessionStorage slot holding the page the user was trying to reach when they were asked to
 *  sign in. The server's OAuth callback always redirects to '/', so the deep link has to be
 *  parked somewhere that survives the round trip through GitHub -- per tab, not per browser. */
export const RETURN_KEY = 'mr_return_to'

// ── Config ───────────────────────────────────────────────────────
export async function getConfig(): Promise<RouterConfig> {
  return json<RouterConfig>('/v1/config')
}

export async function putConfig(cfg: RouterConfig): Promise<void> {
  await ensureOk(await req('/v1/config', jsonBody('PUT', cfg)))
}

/** A rendered preview of the AI decision prompt, produced by the backend through the very same
 *  code path that real routing uses. */
export interface PromptPreview {
  /** The system content that would actually be sent to the decision model. */
  system: string
  catalog: string
  /** The user content after the sample prompt is truncated (empty when no sample was given). */
  user: string
  sample_truncated: boolean
  model_count: number
  candidates: string[]
  decision_model: string
  decision_provider: string
  is_default_prompt: boolean
  has_placeholder: boolean
  models_without_description: string[]
  default_model: string | null
  chars: number
}

/** Render the preview from the **unsaved draft**: models / ai_router are posted as-is, so there is
 *  no need to save first. */
export async function previewDecisionPrompt(payload: {
  models: RouterConfig['models']
  ai_router: RouterConfig['ai_router']
  sample_prompt?: string
}): Promise<PromptPreview> {
  return json<PromptPreview>('/v1/config/decision-prompt/preview', jsonBody('POST', payload))
}

export async function getDefaultDecisionPrompt(): Promise<{ prompt: string; placeholder: string }> {
  return json('/v1/config/decision-prompt/default')
}

/** Write back the auth section only: the backend merges by top-level key, so providers/models/rules
 *  are left untouched. */
export async function putAuthConfig(auth: AuthConfig): Promise<void> {
  await ensureOk(await req('/v1/config', jsonBody('PUT', { auth })))
}

// ── Traces ───────────────────────────────────────────────────────
/** One page of trace summaries. `total` counts everything matching the filters on disk, not the
 *  number returned -- it is what the "N of M" footer and the batch-delete confirmation quote. */
export interface TracePage {
  total: number
  items: TraceSummary[]
  offset: number
  limit: number
  /** true when the server stopped short of scanning every date directory, so `total` is a lower
   *  bound. Narrow the date filter to get an exact count. */
  truncated: boolean
}

export interface TraceQuery {
  limit?: number
  offset?: number
  date?: string
  userId?: string
  traceId?: string
  sessionId?: string
}

export async function getTraces(opts: TraceQuery = {}): Promise<TracePage> {
  const q = new URLSearchParams({
    limit: String(opts.limit ?? 50),
    offset: String(opts.offset ?? 0),
  })
  if (opts.date) q.set('date', opts.date)
  if (opts.userId) q.set('user_id', opts.userId)
  if (opts.traceId) q.set('trace_id', opts.traceId)
  if (opts.sessionId) q.set('session_id', opts.sessionId)
  return json<TracePage>(`/v1/traces?${q}`)
}

export async function getTrace(id: string): Promise<TraceDetail> {
  return json<TraceDetail>(`/v1/traces/${id}`)
}

export async function deleteTrace(id: string): Promise<{ ok: boolean; deleted: number }> {
  return del(`/v1/traces/${id}`)
}

/** Batch delete. At least one criterion is required -- the server rejects an unfiltered call
 *  rather than treating it as "delete everything". */
export async function deleteTraces(opts: { date?: string; userId?: string }): Promise<{ deleted: number }> {
  const q = new URLSearchParams()
  if (opts.date) q.set('date', opts.date)
  if (opts.userId) q.set('user_id', opts.userId)
  return del(`/v1/traces?${q}`)
}

// ── API keys ─────────────────────────────────────────────────────
export async function getKeys(all = false): Promise<ApiKey[]> {
  return json<ApiKey[]>(`/v1/keys${all ? '?all=1' : ''}`)
}

export async function createKey(name: string): Promise<ApiKey> {
  return json<ApiKey>('/v1/keys', jsonBody('POST', { name }))
}

export async function setKeyDisabled(id: string, disabled: boolean): Promise<ApiKey> {
  return json<ApiKey>(`/v1/keys/${id}`, jsonBody('PATCH', { disabled }))
}

export async function deleteKey(id: string): Promise<void> {
  await ensureOk(await req(`/v1/keys/${id}`, { method: 'DELETE' }))
}

// ── Access control ───────────────────────────────────────────────
export interface AccessCheck {
  enterprise: string
  kind: 'organization' | 'team' | 'enterprise' | 'org-scan'
  /** For a team this is the **team name** (the policy stores numeric ids; the backend resolves them
   *  to names before display). */
  name: string
  /** kind='team' only: the team's numeric id, useful when troubleshooting; falls back to the same
   *  value as name when the team list is unavailable. */
  id?: string
  /** null = GitHub cannot answer (enterprise-level checks are unavailable on very large
   *  enterprises); treated as "not a member". */
  member: boolean | null
  scanned?: number
  truncated?: boolean
  /** Which layer answered: 'cache' = a complete local member list, 'probe' = a cached individual
   *  result, 'live' = a GitHub call was made for this check. */
  source?: 'cache' | 'probe' | 'live'
}

export interface AccessVerdict {
  login: string
  is_admin: boolean
  allowed: boolean
  reason: string
  policy_enabled: boolean
  matched: { kind: string; enterprise?: string; name?: string; id?: string } | null
  detail: AccessCheck[]
}

export interface TokenOwner {
  login: string
  name: string
  avatar_url?: string | null
  scopes: string[]
  has_enterprise_scope: boolean
}

export interface TokenStatus {
  configured: boolean
  /** A mask such as ghp_abc…wxyz (a few characters kept at each end). The server never echoes the
   *  plaintext token. */
  hint: string
  owner: TokenOwner | null
  error: string | null
}

export interface DiscoveredEnterprise {
  slug: string
  name: string
  id: string
  organizations: { login: string; name: string }[]
  organizations_total: number
  /** true when the organization count exceeded the cap -- the list is truncated, not complete. */
  organizations_truncated: boolean
  organizations_error: string | null
  teams: { id: number; slug: string; name: string }[]
  /** On some enterprises the Teams endpoint simply 404s, which is different from "has no teams". */
  teams_error: string | null
}

export async function getMyAccess(): Promise<AccessVerdict> {
  return json<AccessVerdict>('/v1/access/me')
}

export async function getTokenStatus(): Promise<TokenStatus> {
  return json<TokenStatus>('/v1/access/token')
}

export async function verifyToken(token?: string): Promise<TokenOwner> {
  return json<TokenOwner>('/v1/access/verify-token', jsonBody('POST', { token: token ?? '' }))
}

export async function discoverEnterprises(
  refresh = false,
): Promise<{ enterprises: DiscoveredEnterprise[]; cached?: boolean; fetched_at?: number }> {
  return json(`/v1/access/discover${refresh ? '?refresh=1' : ''}`)
}

/** The state of the on-disk GitHub cache. Deliberately carries counts and never logins: an org's
 *  member list is not something a status panel should publish. */
export interface CacheScope {
  key: string
  kind: string
  name: string
  count: number
  truncated: boolean
  error: string
  fetched_at: number
}

export interface CacheStatus {
  token_configured: boolean | null
  /** false when the token was replaced since the last refresh, i.e. nothing cached is being
   *  trusted regardless of how fresh it looks. */
  token_matches: boolean | null
  refresh_seconds: number
  structure: {
    fetched_at: number
    age_seconds: number | null
    enterprises: {
      slug: string
      name: string
      organizations: number
      organizations_truncated: boolean
      teams: number
      error: string
    }[]
    error: string
  }
  members: {
    fetched_at: number
    age_seconds: number | null
    scopes: CacheScope[]
    truncated_scopes: number
    errored_scopes: number
  }
  probes: { count: number }
  stale: boolean
  error?: string
}

export async function getCacheStatus(): Promise<CacheStatus> {
  return json<CacheStatus>('/v1/access/cache')
}

export async function refreshCache(): Promise<CacheStatus> {
  return json<CacheStatus>('/v1/access/cache/refresh', { method: 'POST' })
}

// ── Usage ────────────────────────────────────────────────────────
export async function getUsage(days = 7, userId?: string): Promise<UsageReport> {
  const q = new URLSearchParams({ days: String(days) })
  if (userId) q.set('user_id', userId)
  return json<UsageReport>(`/v1/usage?${q}`)
}

// ── Playground ───────────────────────────────────────────────────
export interface ChatResult {
  traceId: string
  model: string
  reason: string
  decisionMs: string
  content: string
}

export async function sendChat(opts: {
  prompt: string
  apiKey: string
  session?: string
  maxTokens: number
  stream: boolean
}): Promise<ChatResult> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${opts.apiKey}`,
  }
  if (opts.session) headers['x-session-id'] = opts.session
  const res = await ensureOk(
    await req('/v1/chat/completions', {
      method: 'POST',
      headers,
      body: JSON.stringify({
        messages: [{ role: 'user', content: opts.prompt }],
        max_tokens: opts.maxTokens,
        stream: opts.stream,
      }),
    }),
  )
  const meta = {
    traceId: res.headers.get('x-trace-id') ?? '',
    model: res.headers.get('x-routed-model') ?? '',
    reason: res.headers.get('x-router-reason') ?? '',
    decisionMs: res.headers.get('x-router-decision-ms') ?? '',
  }
  if (!opts.stream) {
    const body = await res.json()
    return { ...meta, content: body.choices?.[0]?.message?.content ?? '' }
  }
  const reader = res.body!.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let content = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const lines = buffer.split('\n')
    buffer = lines.pop() ?? ''
    for (const line of lines) {
      if (!line.startsWith('data: ') || line === 'data: [DONE]') continue
      try {
        const chunk = JSON.parse(line.slice(6))
        content += chunk.choices?.[0]?.delta?.content ?? ''
      } catch { /* skip incomplete chunks */ }
    }
  }
  return { ...meta, content }
}
