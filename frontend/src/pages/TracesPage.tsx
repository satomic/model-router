import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useNavigate, useParams } from 'react-router-dom'
import { useDialogs } from '../components/Dialog'
import JsonView from '../components/JsonView'
import { formatClock, formatStamp } from '../i18n/format'
import { effectiveTimeZone } from '../i18n/timezone'
import {
  deleteTrace,
  deleteTraces,
  getTrace,
  getTraces,
  type RoutingAnalysis,
  type SessionUser,
  type TraceDetail,
  type TraceSummary,
  type TraceTurn,
} from '../api'

const PAGE_SIZES = [25, 50, 100]
const PAGE_SIZE_DEFAULT = 50
const PAGE_SIZE_KEY = 'mr_traces_page_size'
/** Where the split ratio is remembered. A dragged layout that resets on every navigation is
 *  worse than no drag at all. */
const SPLIT_KEY = 'mr_traces_split'
const SPLIT_DEFAULT = 70
const SPLIT_MIN = 25
const SPLIT_MAX = 85

/** Resizable column widths, remembered for the same reason as the split ratio. Prompt is
 *  deliberately absent: it is the column that absorbs whatever the others leave, so the table
 *  still fills the pane at any split. The admin delete column is a fixed 40px icon.
 *
 *  Versioned: Time gained a date, so widths stored against the old time-only column are stale
 *  rather than a preference worth honouring. */
const COLS_KEY = 'mr_traces_cols_v2'
const COL_DEFAULTS: Record<string, number> = {
  // Content width plus the 24px of cell padding. Time carries a full `2026-09-07 14:30:05`,
  // which is what makes a list spanning several days readable at a glance.
  time: 156, user: 112, model: 104, decision: 116, turns: 52, latency: 66, status: 62,
}
const COL_MIN = 44
const COL_MAX = 480

function AnalysisView({ analysis }: { analysis: RoutingAnalysis }) {
  const { t } = useTranslation()

  if (analysis.type === 'session' || analysis.type === 'gate') {
    return (
      <>
        <div className="mono dim">{analysis.note}</div>
        {analysis.bound_by === 'interaction' && (
          // Worth spelling out: this is the case where the router deliberately did *not* pay
          // for a second decision on a question it had already routed.
          <p className="faint" style={{ margin: '6px 0 0' }}>{t('traces.analysis.interactionReuse')}</p>
        )}
      </>
    )
  }
  if (analysis.type === 'rule-then-ai') {
    // Both strategies were configured. Render each stage with the renderer that stage's own
    // type already has, rather than a third copy of them -- and state which one decided,
    // because the model alone does not say whether a rule fired or a decision call was paid
    // for. `analysis.ai` is absent exactly when a rule matched: no call was made.
    const byRule = analysis.decided_by === 'rule'
    return (
      <>
        <div className={`badge ${byRule ? 'ok' : ''}`} style={{ marginBottom: 8 }}>
          {t(byRule ? 'traces.analysis.decidedByRule' : 'traces.analysis.decidedByAi')}
        </div>
        {analysis.rule && (
          <div className="stage">
            <div className="stage-name">{t('traces.analysis.stageRules')}</div>
            <AnalysisView analysis={analysis.rule} />
          </div>
        )}
        {analysis.ai && (
          <div className="stage">
            <div className="stage-name">{t('traces.analysis.stageAi')}</div>
            <AnalysisView analysis={analysis.ai} />
          </div>
        )}
      </>
    )
  }
  if (analysis.type === 'rule') {
    return (
      <>
        {(analysis.evaluated ?? []).map((s, i) => (
          <div className="step" key={i}>
            <span className={`mark ${s.matched ? 'hit' : 'miss'}`}>{s.matched ? '✓' : '·'}</span>
            <span className="rule-name">{s.rule}</span>
            <span className="dim">→ {s.model}</span>
            <span className="faint">{s.skipped ?? s.check}</span>
            {s.matched_keyword && (
              <span className="badge ok">
                {t('traces.analysis.matchedKeyword', { keyword: s.matched_keyword })}
              </span>
            )}
          </div>
        ))}
        {analysis.fallback && (
          <div className="mono dim" style={{ marginTop: 8 }}>⤷ {String(analysis.fallback)}</div>
        )}
      </>
    )
  }
  return (
    <dl className="kv">
      <dt>{t('traces.analysis.decisionModel')}</dt>
      <dd>
        {analysis.decision_model}
        {analysis.decision_model_version && analysis.decision_model_version !== analysis.decision_model && (
          <span className="dim"> ({analysis.decision_model_version})</span>
        )}
        {analysis.decision_engine === 'typesafe' && (
          <span className="badge ok" style={{ marginLeft: 6 }}>TypeSafe</span>
        )}
      </dd>
      {analysis.probabilities && (
        <>
          <dt>{t('traces.analysis.probabilities')}</dt>
          <dd className="mono">
            {Object.entries(analysis.probabilities)
              .sort(([, a], [, b]) => b - a)
              .map(([name, p]) => `${name} ${(p * 100).toFixed(1)}%`)
              .join(' · ')}
            {analysis.confidence != null && (
              <span className="dim">
                {' '}
                — {t('traces.analysis.confidence', { value: analysis.confidence.toFixed(2) })}
              </span>
            )}
          </dd>
        </>
      )}
      {analysis.rationale && (
        <>
          <dt>{t('traces.analysis.rationale')}</dt>
          <dd style={{ color: 'var(--amber)' }}>{analysis.rationale}</dd>
        </>
      )}
      {analysis.decision_latency_ms != null && (
        <>
          <dt>{t('traces.analysis.decisionLatency')}</dt>
          <dd>{analysis.decision_latency_ms} ms</dd>
        </>
      )}
      {analysis.decision_usage && (
        <>
          <dt>{t('traces.analysis.decisionTokens')}</dt>
          <dd>
            prompt={analysis.decision_usage.prompt_tokens} / completion={analysis.decision_usage.completion_tokens}
          </dd>
        </>
      )}
      <dt>{t('traces.analysis.candidates')}</dt>
      <dd>{(analysis.candidates ?? []).join(', ')}</dd>
      {analysis.decision_system && (
        <>
          {/* The prompt is editable on the Routing strategy page, so the version actually
              sent for this request is recorded here */}
          <dt>{t('traces.analysis.systemPrompt')}</dt>
          <dd>
            <details>
              <summary className="dim" style={{ cursor: 'pointer' }}>
                {t('traces.analysis.charsExpand', { count: analysis.decision_system.length })}
              </summary>
              <pre className="code" style={{ marginTop: 6 }}>{analysis.decision_system}</pre>
            </details>
          </dd>
        </>
      )}
      {analysis.decision_question && (
        <>
          <dt>{t('traces.analysis.decisionQuestion')}</dt>
          <dd>
            <details>
              <summary className="dim" style={{ cursor: 'pointer' }}>
                {t('traces.analysis.optionsExpand', {
                  count: Object.keys(analysis.decision_question.criteria ?? {}).length,
                })}
              </summary>
              <pre className="code" style={{ marginTop: 6 }}>
                {JSON.stringify(analysis.decision_question, null, 2)}
              </pre>
            </details>
          </dd>
        </>
      )}
      {analysis.decision_input && (
        <>
          <dt>{t('traces.analysis.decisionInput')}</dt>
          <dd className="faint">
            {analysis.decision_input}
            {analysis.prompt_truncated ? ` ${t('traces.analysis.truncated')}` : ''}
          </dd>
        </>
      )}
      {analysis.raw_response && (
        <>
          <dt>{t('traces.analysis.rawOutput')}</dt>
          <dd className="faint">{analysis.raw_response}</dd>
        </>
      )}
      {analysis.error && (
        <>
          <dt>{t('common.error')}</dt>
          <dd style={{ color: 'var(--red)' }}>{analysis.error}</dd>
        </>
      )}
      {analysis.fallback && (
        <>
          <dt>{t('traces.analysis.fallback')}</dt>
          <dd style={{ color: 'var(--amber)' }}>{t('traces.analysis.fellBackToDefault')}</dd>
        </>
      )}
    </dl>
  )
}

/** One row of the interaction chain: which upstream call it was, what it cost, and what the
 *  model asked for. Collapsed by default -- a long tool loop otherwise buries the panels below
 *  it -- and expanding shows the turn's own response and, when the client rewrote the
 *  conversation, that turn's message chain. */
function TurnRow({ turn, total }: { turn: TraceTurn; total: number }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const calls = turn.response?.tool_calls ?? []
  const usage = turn.response?.usage as { total_tokens?: number } | null | undefined

  return (
    <div className={`turn ${open ? 'open' : ''}`}>
      <button className="turn-head" type="button" aria-expanded={open} onClick={() => setOpen(!open)}>
        <span className="json-toggle" aria-hidden>{open ? '▾' : '▸'}</span>
        <span className="turn-index mono">{t('traces.turns.nth', { index: turn.index, total })}</span>
        <span className="mono dim">{formatClock(turn.ts)}</span>
        {/* The model is named on every turn precisely so it is visible that it did not change:
            one routing decision for the whole interaction is the point of the record. */}
        <span className="badge model">{turn.model}</span>
        {turn.initiator && <span className="badge">{turn.initiator}</span>}
        <span className="mono dim">
          {turn.total_ms != null ? `${(turn.total_ms / 1000).toFixed(1)}s` : '—'}
        </span>
        {usage?.total_tokens != null && (
          <span className="mono faint">{t('traces.turns.tokens', { count: usage.total_tokens })}</span>
        )}
        <span className="spacer" />
        {calls.length > 0 ? (
          <span className="turn-calls truncate">
            {calls.map((c, i) => (
              <span className="badge ok" key={c.id ?? i}>{c.function?.name ?? c.type ?? 'tool'}</span>
            ))}
          </span>
        ) : (
          <span className="faint">{t('traces.turns.noTools')}</span>
        )}
        {turn.error
          ? <span className="badge error">{t('common.error')}</span>
          : <span className="mono faint">{turn.response?.finish_reason ?? '—'}</span>}
      </button>
      {open && (
        <div className="turn-body">
          <dl className="kv">
            <dt>{t('traces.turns.messageCount')}</dt>
            <dd className="mono">{turn.message_count}</dd>
            {turn.request_id && (
              <>
                <dt>{t('traces.turns.requestId')}</dt>
                <dd className="mono dim">{turn.request_id}</dd>
              </>
            )}
          </dl>
          {turn.error && <div className="toast error" style={{ marginTop: 8 }}>{turn.error}</div>}
          {turn.response?.content && (
            <div className="reply-box" style={{ marginTop: 8 }}>{turn.response.content}</div>
          )}
          {calls.length > 0 && (
            <div style={{ marginTop: 8 }}>
              <div className="field-name" style={{ margin: '0 0 5px' }}>tool_calls</div>
              <JsonView value={calls} />
            </div>
          )}
          {turn.messages && (
            <div style={{ marginTop: 8 }}>
              {/* Only stored when this turn's chain is not a prefix of the record's final one,
                  so saying why it is here keeps it from looking like a duplicate. */}
              <div className="field-name" style={{ margin: '0 0 5px' }}>
                {turn.rewritten ? t('traces.turns.rewritten') : t('traces.turns.superseded')}
              </div>
              <JsonView value={turn.messages} defaultDepth={1} />
            </div>
          )}
          {turn.params && (
            <div style={{ marginTop: 8 }}>
              <div className="field-name" style={{ margin: '0 0 5px' }}>params</div>
              <JsonView value={turn.params} defaultDepth={1} />
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function DetailView({ trace }: { trace: TraceDetail }) {
  const { t } = useTranslation()
  const turns = trace.turns ?? []
  return (
    <div>
      <div className="panel">
        <div className="panel-head">{t('traces.detail.overview')} · {trace.id}</div>
        <div className="panel-body">
          <dl className="kv">
            <dt>{t('traces.detail.time')}</dt>
            <dd>
              <span className="mono">{formatStamp(trace.ts)}</span>{' '}
              {/* Named, because the record itself is UTC and the reader's zone is a preference:
                  a bare wall-clock time would be unresolvable from a screenshot. */}
              <span className="faint">{effectiveTimeZone()}</span>
            </dd>
            <dt>{t('traces.detail.user')}</dt>
            <dd>{trace.user_id ?? '—'}</dd>
            <dt>{t('traces.detail.apiKey')}</dt>
            <dd>
              {trace.api_key_name
                ? t('traces.detail.apiKeyValue', {
                    name: trace.api_key_name,
                    id: trace.api_key_id,
                  })
                : '—'}
            </dd>
            <dt>{t('traces.detail.session')}</dt>
            <dd>
              {trace.session_id ?? '—'}
              {trace.sticky && trace.session_id ? ` ${t('traces.detail.stickySuffix')}` : ''}
            </dd>
            <dt>{t('traces.detail.interaction')}</dt>
            <dd className="mono">
              {trace.interaction_id ?? <span className="faint">{t('traces.detail.noInteraction')}</span>}
            </dd>
            <dt>{t('traces.detail.turns')}</dt>
            <dd>
              {t('traces.detail.turnCount', { count: trace.turn_count ?? 1 })}
              {/* One decision for N calls is the guarantee this record exists to show, so it is
                  stated here rather than left to be inferred from the turn list. */}
              {(trace.turn_count ?? 1) > 1 && ` · ${t('traces.detail.oneDecision')}`}
            </dd>
            <dt>{t('common.status')}</dt>
            <dd><span className={`badge ${trace.status}`}>{trace.status}</span></dd>
            <dt>{t('traces.detail.totalLatency')}</dt>
            <dd>
              {t('traces.detail.latencyBreakdown', {
                total: trace.total_ms,
                decision: trace.routing.decision_ms,
                backend: trace.backend.latency_ms ?? '—',
              })}
            </dd>
          </dl>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          {t('traces.detail.routingDecision')}
          <span className="spacer" />
          <span className="badge model">{trace.routing.model}</span>
          <span className="badge warn">{trace.routing.reason}</span>
        </div>
        <div className="panel-body">
          <AnalysisView analysis={trace.routing.analysis} />
        </div>
      </div>

      {turns.length > 0 && (
        <div className="panel">
          <div className="panel-head">
            {t('traces.detail.chain')}
            <span className="spacer" />
            <span className="badge">{t('traces.detail.turnCount', { count: trace.turn_count ?? turns.length })}</span>
          </div>
          <div className="panel-body">
            {(trace.turn_count ?? turns.length) > 1 && (
              <p className="faint" style={{ margin: '0 0 8px' }}>{t('traces.detail.chainNote')}</p>
            )}
            {turns.map((turn) => (
              <TurnRow key={turn.index} turn={turn} total={trace.turn_count ?? turns.length} />
            ))}
            {trace.turns_truncated ? (
              <p className="faint" style={{ marginTop: 8 }}>
                {t('traces.detail.turnsTruncated', { count: trace.turns_truncated })}
              </p>
            ) : null}
          </div>
        </div>
      )}

      <div className="panel">
        <div className="panel-head">{t('traces.detail.requestParams')}</div>
        <div className="panel-body">
          <JsonView value={trace.request.params} />
          <div className="field-name" style={{ margin: '12px 0 5px' }}>
            messages
            {/* The chain as it stood on the final turn, i.e. every tool call and tool result of
                the interaction -- not just the opening question. */}
            {turns.length > 1 && (
              <span className="faint" style={{ marginLeft: 6, fontWeight: 400 }}>
                {t('traces.detail.messagesFinal')}
              </span>
            )}
          </div>
          <JsonView value={trace.request.messages} />
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          {t('traces.detail.backendCall')}
          <span className="spacer" />
          <span className="badge model">{trace.backend.deployment}</span>
          <span className="badge">{trace.backend.api} api</span>
        </div>
        <div className="panel-body">
          <JsonView value={trace.backend.sent_params} />
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          {t('traces.detail.modelResponse')}
          {/* Which of the turns this is, so the single reply box is not read as the whole
              interaction's output. */}
          {turns.length > 1 && <span className="faint" style={{ marginLeft: 6 }}>{t('traces.detail.finalTurn')}</span>}
        </div>
        <div className="panel-body">
          {trace.error ? (
            <div className="toast error">{trace.error}</div>
          ) : trace.response ? (
            <>
              <div className="reply-box">
                {trace.response.content || t('common.emptyContent')}
              </div>
              <dl className="kv" style={{ marginTop: 12 }}>
                <dt>finish_reason</dt>
                <dd>{trace.response.finish_reason ?? '—'}</dd>
              </dl>
            </>
          ) : (
            <div className="empty">{t('traces.detail.noResponse')}</div>
          )}
          {/* Outside the response branch: an interaction whose last turn failed still spent
              everything the earlier turns spent, and reporting nothing would read as free. */}
          {(trace.usage ?? trace.response?.usage) && (
            <div style={{ marginTop: 8 }}>
              <div className="field-name" style={{ margin: '0 0 5px' }}>
                usage
                {turns.length > 1 && (
                  <span className="faint" style={{ marginLeft: 6, fontWeight: 400 }}>
                    {t('traces.detail.usageSummed', { count: trace.turn_count ?? turns.length })}
                  </span>
                )}
              </div>
              <JsonView value={trace.usage ?? trace.response?.usage} />
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

/** The draggable divider. The listeners go on `document`, not on the handle: once the pointer is
 *  down, the cursor routinely leaves the 6px strip, and a handle-scoped mousemove would drop the
 *  drag the moment it did. Same pattern as the user menu in Shell.tsx. */
function Splitter({ onDrag }: { onDrag: (clientX: number) => void }) {
  function begin(e: React.MouseEvent) {
    e.preventDefault()
    document.body.classList.add('dragging')
    const move = (ev: MouseEvent) => onDrag(ev.clientX)
    const up = () => {
      document.removeEventListener('mousemove', move)
      document.removeEventListener('mouseup', up)
      document.body.classList.remove('dragging')
    }
    document.addEventListener('mousemove', move)
    document.addEventListener('mouseup', up)
  }
  return (
    <div className="splitter" onMouseDown={begin} role="separator" aria-orientation="vertical" />
  )
}

/** The trace being viewed lives in the URL as /traces/<id>, so a single request is a link one
 *  can paste into a ticket. Note the server 404s a trace belonging to somebody else, which is
 *  why "not found" is a rendered state and not merely a swallowed error. */
export default function TracesPage({ user }: { user: SessionUser }) {
  const { t } = useTranslation()
  const dialogs = useDialogs()
  const { traceId } = useParams()
  const navigate = useNavigate()
  const [list, setList] = useState<TraceSummary[]>([])
  const [total, setTotal] = useState(0)
  const [truncated, setTruncated] = useState(false)
  const [loading, setLoading] = useState(false)
  const [selected, setSelected] = useState<TraceDetail | null>(null)
  const [missing, setMissing] = useState(false)
  const [auto, setAuto] = useState(true)
  const [error, setError] = useState('')

  // Filters. `date` and the two text boxes are separate state from `applied` so the text inputs
  // can be debounced without the caret jumping while a request is in flight.
  const [date, setDate] = useState('')
  const [traceFilter, setTraceFilter] = useState('')
  const [userFilter, setUserFilter] = useState('')
  const [applied, setApplied] = useState({ date: '', traceId: '', userId: '' })
  // Mirrors `applied` for the debounce to compare against, without making the timer effect
  // depend on it (which would restart the debounce on every applied change).
  const appliedRef = useRef(applied)

  // True pagination rather than an ever-growing list: one page is one request, and the cost of
  // reading page 40 is the same as reading page 1.
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(() => {
    const stored = Number(localStorage.getItem(PAGE_SIZE_KEY))
    return PAGE_SIZES.includes(stored) ? stored : PAGE_SIZE_DEFAULT
  })

  const [split, setSplit] = useState(() => {
    const stored = Number(localStorage.getItem(SPLIT_KEY))
    return stored >= SPLIT_MIN && stored <= SPLIT_MAX ? stored : SPLIT_DEFAULT
  })
  const splitRef = useRef<HTMLDivElement>(null)

  const [cols, setCols] = useState<Record<string, number>>(() => {
    try {
      const stored = JSON.parse(localStorage.getItem(COLS_KEY) ?? '{}') as Record<string, unknown>
      const out = { ...COL_DEFAULTS }
      for (const key of Object.keys(COL_DEFAULTS)) {
        const w = Number(stored[key])
        if (w >= COL_MIN && w <= COL_MAX) out[key] = w
      }
      return out
    } catch {
      // A hand-edited or stale entry should cost the defaults, not the page.
      return { ...COL_DEFAULTS }
    }
  })

  /** Drag the right edge of a header cell. Listeners go on `document` for the same reason as the
   *  splitter's: the pointer leaves the 6px strip almost immediately. */
  function beginColResize(key: string, e: React.MouseEvent) {
    e.preventDefault()
    const startX = e.clientX
    const startWidth = cols[key] ?? COL_DEFAULTS[key]
    document.body.classList.add('dragging')
    let width = startWidth
    const move = (ev: MouseEvent) => {
      width = Math.min(COL_MAX, Math.max(COL_MIN, Math.round(startWidth + ev.clientX - startX)))
      setCols((prev) => ({ ...prev, [key]: width }))
    }
    const up = () => {
      document.removeEventListener('mousemove', move)
      document.removeEventListener('mouseup', up)
      document.body.classList.remove('dragging')
      // `cols` is the value from drag start, and this drag only ever changed `key`.
      localStorage.setItem(COLS_KEY, JSON.stringify({ ...cols, [key]: width }))
    }
    document.addEventListener('mousemove', move)
    document.addEventListener('mouseup', up)
  }

  /** Double-click restores one column, so a drag that went wrong does not need a pixel-perfect
   *  drag back. */
  function resetCol(key: string) {
    setCols((prev) => {
      const next = { ...prev, [key]: COL_DEFAULTS[key] }
      localStorage.setItem(COLS_KEY, JSON.stringify(next))
      return next
    })
  }

  const colHandle = (key: string) => (
    <span
      className="col-resize"
      role="separator"
      aria-orientation="vertical"
      onMouseDown={(e) => beginColResize(key, e)}
      onDoubleClick={() => resetCol(key)}
    />
  )

  // Debounce the text filters: a filter change refetches, and refetching on every keystroke of a
  // trace id would be one request per character.
  useEffect(() => {
    const next = { date, traceId: traceFilter.trim(), userId: userFilter.trim() }
    const timer = setTimeout(() => {
      const prev = appliedRef.current
      // The debounce fires once on mount with the values it already holds. Comparing rather than
      // setting keeps `applied` identity stable, so that tick costs no request and no page reset.
      if (prev.date === next.date && prev.traceId === next.traceId && prev.userId === next.userId)
        return
      appliedRef.current = next
      // Both in one tick, so the loader sees the new filter and page 1 together; setting the page
      // from its own effect fetched the stale page number first.
      setApplied(next)
      setPage(0)
    }, 300)
    return () => clearTimeout(timer)
  }, [date, traceFilter, userFilter])

  /** Load the current page, always replacing the list. */
  const load = useCallback(() => {
    setLoading(true)
    getTraces({ ...applied, limit: pageSize, offset: page * pageSize })
      .then((res) => {
        setList(res.items)
        setTotal(res.total)
        setTruncated(res.truncated)
        setError('')
      })
      .catch((e) => setError(String(e)))
      .finally(() => setLoading(false))
  }, [applied, page, pageSize])

  useEffect(() => {
    load()
  }, [load])

  // The live tail follows the newest records, which is the first page by definition -- so it goes
  // quiet while a later page is being read rather than shifting rows out from under the reader.
  useEffect(() => {
    if (!auto || page !== 0) return
    const timer = setInterval(load, 5000)
    return () => clearInterval(timer)
  }, [auto, page, load])

  const pageCount = Math.max(1, Math.ceil(total / pageSize))

  // A delete can shorten the result set past the page being viewed.
  useEffect(() => {
    if (page > 0 && page >= pageCount) setPage(pageCount - 1)
  }, [page, pageCount])

  function onDrag(clientX: number) {
    const box = splitRef.current?.getBoundingClientRect()
    if (!box || box.width <= 0) return
    const pct = ((clientX - box.left) / box.width) * 100
    const clamped = Math.min(SPLIT_MAX, Math.max(SPLIT_MIN, pct))
    setSplit(clamped)
    localStorage.setItem(SPLIT_KEY, String(Math.round(clamped)))
  }

  async function removeOne(id: string) {
    const yes = await dialogs.confirm({
      title: t('traces.delete.confirmOneTitle'),
      message: t('traces.delete.confirmOne', { id }),
      confirmLabel: t('common.delete'),
      danger: true,
    })
    if (!yes) return
    try {
      await deleteTrace(id)
      // The detail pane would otherwise keep showing a trace that no longer exists.
      if (traceId === id) navigate('/traces')
      load()
    } catch (e) {
      setError(String(e))
    }
  }

  async function removeFiltered() {
    const criteria = [
      applied.date && t('traces.delete.criteriaDate', { date: applied.date }),
      applied.userId && t('traces.delete.criteriaUser', { user: applied.userId }),
    ].filter(Boolean).join(', ')
    const yes = await dialogs.confirm({
      title: t('traces.delete.confirmManyTitle'),
      message: t('traces.delete.confirmMany', { count: total, criteria }),
      confirmLabel: t('traces.delete.filtered'),
      danger: true,
    })
    if (!yes) return
    try {
      const { deleted } = await deleteTraces({ date: applied.date, userId: applied.userId })
      if (traceId) navigate('/traces')
      setPage(0)
      load()
      setError('')
      await dialogs.alert({
        title: t('traces.delete.doneTitle'),
        message: t('traces.delete.done', { count: deleted }),
      })
    } catch (e) {
      setError(String(e))
    }
  }

  // A batch delete needs a criterion the *server* honours. A non-admin's user_id is overwritten
  // server-side, so only date and (for an admin) user_id count here.
  const canDeleteFiltered = user.is_admin && Boolean(applied.date || applied.userId)

  // Read off the live inputs rather than `applied`, so the clear button appears as soon as
  // something is typed instead of 300ms later, once the debounce has fired.
  const filtering = Boolean(date || traceFilter || userFilter)

  // The one place a detail is fetched, keyed on the URL. `alive` drops the answer to a request
  // whose id is no longer the one on screen, so a fast click-through cannot land out of order.
  useEffect(() => {
    if (!traceId) {
      setSelected(null)
      setMissing(false)
      return
    }
    let alive = true
    setMissing(false)
    getTrace(traceId)
      .then((d) => {
        if (alive) setSelected(d)
      })
      .catch(() => {
        if (!alive) return
        setSelected(null)
        setMissing(true)
      })
    return () => {
      alive = false
    }
  }, [traceId])

  const listPanel = (
    <div className="panel" style={{ marginBottom: 0 }}>
      <div className="panel-head">
        {t('traces.list.title')}
        <span className="dim" style={{ fontWeight: 400 }}>
          {' '}
          {t('traces.paging.total', { count: total })}
          {truncated ? ` ${t('traces.paging.truncated')}` : ''}
        </span>
        <span className="spacer" />
        <label className="check" title={page > 0 ? t('traces.list.autoRefreshFirstPage') : undefined}>
          <input
            type="checkbox"
            checked={auto}
            disabled={page > 0}
            onChange={(e) => setAuto(e.target.checked)}
          />
          {' '}
          {t('traces.list.autoRefresh')}
        </label>
        {canDeleteFiltered && (
          <button className="btn ghost sm danger" onClick={removeFiltered}>
            {t('traces.delete.filtered')}
          </button>
        )}
        <button className="btn ghost sm" onClick={() => load()}>
          {t('common.refresh')}
        </button>
      </div>

      {/* Filters. The user box is admin-only: a normal user's user_id is overwritten
          server-side, so offering the control would be a lie. */}
      <div className="filter-bar">
        <label className="filter-field">
          <span className="field-name">{t('traces.filter.date')}</span>
          <input type="date" value={date} onChange={(e) => setDate(e.target.value)} />
        </label>
        <label className="filter-field">
          <span className="field-name">{t('traces.filter.traceId')}</span>
          <input
            type="text"
            value={traceFilter}
            placeholder={t('traces.filter.traceIdHint')}
            onChange={(e) => setTraceFilter(e.target.value)}
          />
        </label>
        {user.is_admin && (
          <label className="filter-field">
            <span className="field-name">{t('traces.filter.user')}</span>
            <input
              type="text"
              value={userFilter}
              placeholder={t('traces.filter.userHint')}
              onChange={(e) => setUserFilter(e.target.value)}
            />
          </label>
        )}
        {filtering && (
          <button
            className="btn ghost sm"
            onClick={() => { setDate(''); setTraceFilter(''); setUserFilter('') }}
          >
            {t('traces.filter.clear')}
          </button>
        )}
      </div>

      {error && <div className="toast error" style={{ margin: 10 }}>{error}</div>}
      {list.length === 0 ? (
        <div className="empty">
          {loading
            ? t('common.loading')
            /* Which emptiness this is matters: "none yet, go make a request" is wrong and
               misleading when a filter is what emptied the list, and it hides the fix. */
            : filtering
              ? t('traces.list.emptyFiltered')
              : t('traces.list.empty')}
        </div>
      ) : (
        /* The columns have fixed widths, so a narrow split cannot shrink the table below their
           sum. Without a scroll container of its own it would paint outside the panel and show
           through the gaps between the detail pane's cards. */
        <div className="table-scroll">
        <table>
          {/* Widths come from `cols`, which the header handles drag. Prompt is left unsized so it
              takes what is left, and the narrow-viewport rule drops the pane to one column. */}
          <colgroup>
            <col style={{ width: cols.time }} />
            <col style={{ width: cols.user }} />
            <col style={{ width: cols.model }} />
            <col style={{ width: cols.decision }} />
            <col style={{ width: cols.turns }} />
            <col style={{ width: cols.latency }} />
            <col style={{ width: cols.status }} />
            <col />
            {user.is_admin && <col style={{ width: 40 }} />}
          </colgroup>
          <thead>
            <tr>
              <th>{t('traces.table.time')}{colHandle('time')}</th>
              <th>{t('traces.table.user')}{colHandle('user')}</th>
              <th>{t('traces.table.model')}{colHandle('model')}</th>
              <th>{t('traces.table.decision')}{colHandle('decision')}</th>
              <th title={t('traces.table.turnsHint')}>
                {t('traces.table.turns')}{colHandle('turns')}
              </th>
              <th>{t('traces.table.latency')}{colHandle('latency')}</th>
              <th>{t('common.status')}{colHandle('status')}</th>
              <th>Prompt</th>
              {user.is_admin && <th />}
            </tr>
          </thead>
          <tbody>
            {list.map((row) => (
              // Highlight from the URL rather than the fetched detail, so the row lights up
              // on click instead of after the round trip.
              <tr
                key={row.id}
                className={traceId === row.id ? 'selected' : ''}
                onClick={() => navigate(`/traces/${row.id}`)}
              >
                <td className="mono truncate dim">{formatStamp(row.ts)}</td>
                <td className="mono truncate dim">{row.user_id ?? '—'}</td>
                <td className="truncate"><span className="badge model">{row.model}</span></td>
                <td className="mono truncate dim">{row.reason}</td>
                {/* A count of 1 is the ordinary case and is left plain; anything more means an
                    agentic tool loop folded into this one record, which is worth spotting from
                    the list. */}
                <td className="mono truncate">
                  {(row.turn_count ?? 1) > 1
                    ? <span className="badge">{`×${row.turn_count}`}</span>
                    : <span className="dim">1</span>}
                </td>
                <td className="mono truncate">
                  {row.total_ms != null ? `${(row.total_ms / 1000).toFixed(1)}s` : '—'}
                </td>
                <td className="truncate"><span className={`badge ${row.status}`}>{row.status}</span></td>
                <td className="truncate dim">{row.prompt_preview}</td>
                {user.is_admin && (
                  <td>
                    {/* stopPropagation, or deleting a row would also navigate to it */}
                    <button
                      className="btn-link danger"
                      title={t('traces.delete.one')}
                      onClick={(e) => { e.stopPropagation(); removeOne(row.id) }}
                    >
                      ✕
                    </button>
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
        </div>
      )}

      {list.length > 0 && (
        <div className="list-footer">
          <span className="dim">
            {t('traces.paging.range', {
              from: page * pageSize + 1,
              to: page * pageSize + list.length,
              total,
            })}
            {truncated ? ` ${t('traces.paging.truncated')}` : ''}
          </span>
          <label className="pager-size">
            {t('traces.paging.perPage')}
            <select
              value={pageSize}
              disabled={loading}
              onChange={(e) => {
                const next = Number(e.target.value)
                localStorage.setItem(PAGE_SIZE_KEY, String(next))
                // Page 7 of 25-row pages is not page 7 of 100-row pages, so the position goes
                // back to the top rather than somewhere the reader did not ask for.
                setPageSize(next)
                setPage(0)
              }}
            >
              {PAGE_SIZES.map((n) => (
                <option key={n} value={n}>{n}</option>
              ))}
            </select>
          </label>
          <span className="spacer" />
          <div className="pager">
            <button
              className="btn ghost sm"
              disabled={page === 0 || loading}
              onClick={() => setPage(0)}
              title={t('traces.paging.first')}
              aria-label={t('traces.paging.first')}
            >
              «
            </button>
            <button
              className="btn ghost sm"
              disabled={page === 0 || loading}
              onClick={() => setPage((p) => p - 1)}
            >
              ‹ {t('traces.paging.prev')}
            </button>
            <span className="dim nowrap">
              {t('traces.paging.pageOf', { page: page + 1, pages: pageCount })}
            </span>
            <button
              className="btn ghost sm"
              disabled={page + 1 >= pageCount || loading}
              onClick={() => setPage((p) => p + 1)}
            >
              {t('traces.paging.next')} ›
            </button>
            <button
              className="btn ghost sm"
              disabled={page + 1 >= pageCount || loading}
              onClick={() => setPage(pageCount - 1)}
              title={t('traces.paging.last')}
              aria-label={t('traces.paging.last')}
            >
              »
            </button>
          </div>
        </div>
      )}
    </div>
  )

  // No selection means no right pane at all -- not an empty one. An always-mounted pane spends
  // 30% of the window on the words "pick a trace".
  if (!traceId) return <div className="layout-single">{listPanel}</div>

  return (
    <div
      className="split-drag"
      ref={splitRef}
      style={{ gridTemplateColumns: `${split}% 6px 1fr` }}
    >
      {listPanel}
      <Splitter onDrag={onDrag} />
      <div className="split-pane">
        {/* Outside the `selected` branch: closing has to be reachable from the loading and
            not-found states too, which is where a stale link lands. */}
        <div className="detail-topbar">
          <span className="mono dim truncate">{traceId}</span>
          <span className="spacer" />
          <button
            className="btn ghost sm"
            title={t('common.close')}
            aria-label={t('common.close')}
            onClick={() => navigate('/traces')}
          >
            ✕
          </button>
        </div>
        {selected ? (
          <DetailView trace={selected} />
        ) : (
          <div className="panel">
            <div className="empty">
              {missing ? t('traces.detail.notFound') : t('common.loading')}
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
