import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useNavigate, useParams } from 'react-router-dom'
import JsonView from '../components/JsonView'
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

const PAGE_SIZE = 50
/** Where the split ratio is remembered. A dragged layout that resets on every navigation is
 *  worse than no drag at all. */
const SPLIT_KEY = 'fmr_traces_split'
const SPLIT_DEFAULT = 70
const SPLIT_MIN = 25
const SPLIT_MAX = 85

function AnalysisView({ analysis }: { analysis: RoutingAnalysis }) {
  const { t } = useTranslation()

  if (analysis.type === 'session') {
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
      <dd>{analysis.decision_model}</dd>
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
        <span className="mono dim">{turn.ts?.slice(11, 19)}</span>
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
            <dd>{trace.ts}</dd>
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

  const [split, setSplit] = useState(() => {
    const stored = Number(localStorage.getItem(SPLIT_KEY))
    return stored >= SPLIT_MIN && stored <= SPLIT_MAX ? stored : SPLIT_DEFAULT
  })
  const splitRef = useRef<HTMLDivElement>(null)

  // Debounce the text filters: a filter change refetches, and refetching on every keystroke of a
  // trace id would be one request per character.
  useEffect(() => {
    const timer = setTimeout(
      () => setApplied({ date, traceId: traceFilter.trim(), userId: userFilter.trim() }),
      300,
    )
    return () => clearTimeout(timer)
  }, [date, traceFilter, userFilter])

  /** Load one page. `append` false replaces the list, which is what every filter change and every
   *  auto-refresh does; true is only the "load more" button. */
  const load = useCallback(
    (offset: number, append: boolean) => {
      setLoading(true)
      getTraces({ ...applied, limit: PAGE_SIZE, offset })
        .then((page) => {
          setList((prev) => (append ? [...prev, ...page.items] : page.items))
          setTotal(page.total)
          setTruncated(page.truncated)
          setError('')
        })
        .catch((e) => setError(String(e)))
        .finally(() => setLoading(false))
    },
    [applied],
  )

  // The first page, and a fresh first page whenever the filters change.
  useEffect(() => {
    load(0, false)
  }, [load])

  // Auto refresh reloads **page 0 only**. Re-fetching an accumulated twenty pages every five
  // seconds is exactly the cost this rework exists to remove. Kept in its own effect, keyed on
  // `auto` alone: folding it into the one above would make toggling the checkbox reset the list to
  // page 0, so turning the tail *off* to keep an appended page would have thrown that page away.
  useEffect(() => {
    if (!auto) return
    const timer = setInterval(() => load(0, false), 5000)
    return () => clearInterval(timer)
  }, [auto, load])

  function onDrag(clientX: number) {
    const box = splitRef.current?.getBoundingClientRect()
    if (!box || box.width <= 0) return
    const pct = ((clientX - box.left) / box.width) * 100
    const clamped = Math.min(SPLIT_MAX, Math.max(SPLIT_MIN, pct))
    setSplit(clamped)
    localStorage.setItem(SPLIT_KEY, String(Math.round(clamped)))
  }

  async function removeOne(id: string) {
    if (!window.confirm(t('traces.delete.confirmOne', { id }))) return
    try {
      await deleteTrace(id)
      // The detail pane would otherwise keep showing a trace that no longer exists.
      if (traceId === id) navigate('/traces')
      load(0, false)
    } catch (e) {
      setError(String(e))
    }
  }

  async function removeFiltered() {
    const criteria = [
      applied.date && t('traces.delete.criteriaDate', { date: applied.date }),
      applied.userId && t('traces.delete.criteriaUser', { user: applied.userId }),
    ].filter(Boolean).join(', ')
    if (!window.confirm(t('traces.delete.confirmMany', { count: total, criteria }))) return
    try {
      const { deleted } = await deleteTraces({ date: applied.date, userId: applied.userId })
      if (traceId) navigate('/traces')
      load(0, false)
      setError('')
      window.alert(t('traces.delete.done', { count: deleted }))
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
          {t('traces.paging.count', { shown: list.length, total })}
          {truncated ? ` ${t('traces.paging.truncated')}` : ''}
        </span>
        <span className="spacer" />
        <label className="check">
          <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
          {' '}
          {t('traces.list.autoRefresh')}
        </label>
        {canDeleteFiltered && (
          <button className="btn ghost sm danger" onClick={removeFiltered}>
            {t('traces.delete.filtered')}
          </button>
        )}
        <button className="btn ghost sm" onClick={() => load(0, false)}>
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
        <table>
          {/* Widths are content width plus the 24px of cell padding: 60px for an 8-character
              mono timestamp leaves 36px, which ellipsises every row to "09:0…". Prompt takes
              what is left, and the narrow-viewport rule drops the pane to one column. */}
          <colgroup>
            <col style={{ width: 88 }} />
            <col style={{ width: 112 }} />
            <col style={{ width: 104 }} />
            <col style={{ width: 116 }} />
            <col style={{ width: 52 }} />
            <col style={{ width: 66 }} />
            <col style={{ width: 62 }} />
            <col />
            {user.is_admin && <col style={{ width: 40 }} />}
          </colgroup>
          <thead>
            <tr>
              <th>{t('traces.table.time')}</th>
              <th>{t('traces.table.user')}</th>
              <th>{t('traces.table.model')}</th>
              <th>{t('traces.table.decision')}</th>
              <th title={t('traces.table.turnsHint')}>{t('traces.table.turns')}</th>
              <th>{t('traces.table.latency')}</th>
              <th>{t('common.status')}</th>
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
                <td className="mono truncate dim">{row.ts?.slice(11, 19)}</td>
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
      )}

      {list.length > 0 && (
        <div className="list-footer">
          <span className="dim">{t('traces.paging.count', { shown: list.length, total })}</span>
          <span className="spacer" />
          {list.length < total && (
            <button
              className="btn ghost sm"
              disabled={loading}
              onClick={() => {
                // Paging and live-tail cannot both be on: the auto-refresh tick reloads page 0 and
                // replaces the list, so an appended page would vanish a few seconds after arriving
                // and the button would look broken. Unticking the box says why the tail stopped and
                // leaves the user free to turn it back on, which returns to page 0.
                setAuto(false)
                load(list.length, true)
              }}
            >
              {loading ? t('common.loading') : t('traces.paging.loadMore', { count: PAGE_SIZE })}
            </button>
          )}
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
