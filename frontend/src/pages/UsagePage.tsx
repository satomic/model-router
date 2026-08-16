import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router-dom'
import { getUsage, type SessionUser, type UsageReport } from '../api'
import { formatInt } from '../i18n/format'

/**
 * Abbreviate large counts so a tile's value never wraps. K/M are kept unlocalised on
 * purpose -- they are read as symbols next to a number, and swapping in per-language
 * units (CJK myriad/eok groupings) would change the *magnitude* each tile shows, not just
 * its label.
 */
const compact = (n: number) =>
  n >= 1_000_000
    ? `${(n / 1_000_000).toFixed(1)}M`
    : n >= 10_000
      ? `${(n / 1000).toFixed(1)}K`
      : formatInt(n)

/** Module-level, so these hold day counts and translation keys, not finished labels. */
const RANGES = [1, 7, 30, 90]

const DEFAULT_DAYS = 7

/** Usage detail. Regular users see only their own; admins can drill into one user.
 *
 *  The range and the drill-down user live in the query string, so "last 30 days for octocat"
 *  is a link -- and Back undoes a drill-down, which was previously a dead end.
 */
export default function UsagePage({ user }: { user: SessionUser }) {
  const { t } = useTranslation()
  const [params, setParams] = useSearchParams()
  const [report, setReport] = useState<UsageReport | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)

  // An unparsable or unoffered ?days= falls back rather than asking the backend for nonsense.
  const asked = Number(params.get('days'))
  const days = RANGES.includes(asked) ? asked : DEFAULT_DAYS
  const focusUser = params.get('user') ?? ''

  /** Replace rather than push: switching the range is a refinement of the same view, so it
   *  should not take a Back press per click to leave the page. */
  const setQuery = (patch: { days?: number; user?: string }) => {
    const next = new URLSearchParams(params)
    for (const [k, v] of Object.entries(patch)) {
      if (v === '' || v === undefined || (k === 'days' && v === DEFAULT_DAYS)) next.delete(k)
      else next.set(k, String(v))
    }
    setParams(next, { replace: true })
  }

  const load = useCallback(() => {
    setLoading(true)
    getUsage(days, focusUser || undefined)
      .then(setReport)
      .catch((e) => setError(String(e)))
      .finally(() => setLoading(false))
  }, [days, focusUser])

  useEffect(() => {
    void load()
  }, [load])

  if (error) return <div className="toast error">{error}</div>
  if (!report) {
    return <div className="empty">{loading ? t('usage.loading') : t('common.noData')}</div>
  }

  const totals = report.totals
  const errorRate = totals.requests ? (totals.errors / totals.requests) * 100 : 0
  const maxModel = Math.max(1, ...report.by_model.map((m) => m.requests))
  const maxDay = Math.max(1, ...report.by_day.map((d) => d.requests))

  return (
    <div>
      <div className="cmdbar" style={{ border: 'none', padding: '0 0 14px' }}>
        {RANGES.map((d) => (
          <button
            key={d}
            className={`btn ${days === d ? '' : 'ghost'} sm`}
            onClick={() => setQuery({ days: d })}
          >
            {d === 1 ? t('usage.range.today') : t('usage.range.lastDays', { count: d })}
          </button>
        ))}
        <span className="spacer" />
        {report.is_admin && (
          <>
            <span className="dim" style={{ fontSize: 12.5 }}>{t('usage.scope')}</span>
            <select
              value={focusUser}
              style={{ width: 200 }}
              onChange={(e) => setQuery({ user: e.target.value })}
            >
              <option value="">{t('usage.allUsers')}</option>
              {report.by_user.map((u) => (
                <option key={u.user_id} value={u.user_id}>{u.user_id}</option>
              ))}
            </select>
          </>
        )}
        <button className="btn subtle sm" onClick={load}>{t('common.refresh')}</button>
      </div>

      <div className="tiles">
        <div className="tile">
          <div className="label">{t('usage.tile.requests')}</div>
          <div className="value">{compact(totals.requests)}</div>
          <div className="foot">
            {report.scope === 'all' ? t('usage.allUsers') : report.scope}
            {' · '}
            {t('usage.range.lastDays', { count: report.days })}
          </div>
        </div>
        <div className="tile">
          <div className="label">{t('usage.tile.totalTokens')}</div>
          <div className="value">{compact(totals.total_tokens)}</div>
          <div className="foot">
            {t('usage.tile.tokenSplit', {
              prompt: compact(totals.prompt_tokens),
              completion: compact(totals.completion_tokens),
            })}
          </div>
        </div>
        <div className="tile">
          <div className="label">{t('usage.tile.errorRate')}</div>
          <div className="value">{errorRate.toFixed(1)}%</div>
          <div className="foot">{t('usage.tile.failures', { count: totals.errors })}</div>
        </div>
        <div className="tile">
          <div className="label">{t('usage.tile.avgLatency')}</div>
          <div className="value">
            {totals.avg_ms === null ? '—' : `${Math.round(totals.avg_ms)}ms`}
          </div>
          <div className="foot">
            P95 {totals.p95_ms === null ? '—' : `${Math.round(totals.p95_ms)}ms`}
          </div>
        </div>
      </div>

      <div className="row" style={{ alignItems: 'flex-start' }}>
        <div style={{ flex: 1, minWidth: 320 }}>
          <div className="panel">
            <div className="panel-head">{t('usage.chart.byModel')}</div>
            <div className="panel-body">
              {report.by_model.length === 0 ? (
                <div className="empty">{t('usage.noRecords')}</div>
              ) : (
                <div className="bars">
                  {report.by_model.map((m) => (
                    <div
                      className="bar-row"
                      key={m.model}
                      title={t('usage.tooltip.model', {
                        model: m.model,
                        count: m.requests,
                      })}
                    >
                      <span className="cat">{m.model}</span>
                      <div className="bar-track">
                        <div
                          className="bar-fill"
                          style={{ width: `${(m.requests / maxModel) * 100}%` }}
                        />
                      </div>
                      <span className="val">{formatInt(m.requests)}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          </div>
        </div>

        <div style={{ flex: 1, minWidth: 320 }}>
          <div className="panel">
            <div className="panel-head">{t('usage.chart.byDay')}</div>
            <div className="panel-body">
              {report.by_day.length === 0 ? (
                <div className="empty">{t('usage.noRecords')}</div>
              ) : (
                <>
                  <div className="col-chart">
                    {report.by_day.map((d) => (
                      <div
                        className="col-wrap"
                        key={d.date}
                        title={t('usage.tooltip.day', {
                          date: d.date,
                          count: d.requests,
                          tokens: formatInt(d.total_tokens),
                          errors: d.errors,
                        })}
                      >
                        <div className="col-cap">{d.requests}</div>
                        <div
                          className="col-bar"
                          style={{ height: `${Math.max(2, (d.requests / maxDay) * 100)}%` }}
                        />
                      </div>
                    ))}
                  </div>
                  <div className="col-axis">
                    {report.by_day.map((d) => (
                      <span className="tick" key={d.date}>{d.date.slice(5)}</span>
                    ))}
                  </div>
                </>
              )}
            </div>
          </div>
        </div>
      </div>

      {report.is_admin && !focusUser && (
        <div className="panel">
          <div className="panel-head">{t('usage.byUser')}</div>
          <div className="panel-body" style={{ padding: 0 }}>
            {report.by_user.length === 0 ? (
              <div className="empty">{t('usage.noRecords')}</div>
            ) : (
              <table>
                <colgroup>
                  <col style={{ width: '46%' }} />
                  <col style={{ width: '18%' }} />
                  <col style={{ width: '22%' }} />
                  <col style={{ width: '14%' }} />
                </colgroup>
                <thead>
                  <tr>
                    <th>{t('usage.table.user')}</th>
                    <th className="num">{t('usage.table.requests')}</th>
                    <th className="num">{t('usage.table.totalTokens')}</th>
                    <th>{t('common.actions')}</th>
                  </tr>
                </thead>
                <tbody>
                  {report.by_user.map((u) => (
                    <tr key={u.user_id} onClick={() => setQuery({ user: u.user_id })}>
                      <td className="truncate">{u.user_id}</td>
                      <td className="num">{formatInt(u.requests)}</td>
                      <td className="num">{formatInt(u.total_tokens)}</td>
                      <td className="nowrap">
                        <button className="btn subtle sm">{t('usage.drillDown')}</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </div>
      )}

      <div className="panel">
        <div className="panel-head">{t('usage.table.title')}</div>
        <div className="panel-body" style={{ padding: 0 }}>
          <table>
            <colgroup>
              <col style={{ width: '28%' }} />
              <col style={{ width: '22%' }} />
              <col style={{ width: '30%' }} />
              <col style={{ width: '20%' }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t('usage.table.date')}</th>
                <th className="num">{t('usage.table.requests')}</th>
                <th className="num">{t('usage.table.totalTokens')}</th>
                <th className="num">{t('usage.table.failures')}</th>
              </tr>
            </thead>
            <tbody>
              {report.by_day.map((d) => (
                <tr key={d.date} style={{ cursor: 'default' }}>
                  <td className="mono">{d.date}</td>
                  <td className="num">{formatInt(d.requests)}</td>
                  <td className="num">{formatInt(d.total_tokens)}</td>
                  <td className="num">{d.errors}</td>
                </tr>
              ))}
              {report.by_day.length === 0 && (
                <tr style={{ cursor: 'default' }}>
                  <td colSpan={4} className="dim">{t('usage.noRecords')}</td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  )
}
