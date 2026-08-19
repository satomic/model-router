import { useCallback, useEffect, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import { getAvailableModels, type AvailableModels } from '../api'

/** What models the signed-in user may actually call, and why.
 *
 *  Not a copy of the model catalog: it asks the backend to resolve the model policy through the
 *  very same code path a real request takes, so this page cannot claim a model that
 *  /v1/chat/completions would then refuse. It is deliberately available to every signed-in user --
 *  the whole point of the policy is that people can see their own curated list.
 *
 *  `reason` is rendered rather than hidden. "You can use everything" and "nothing is configured for
 *  you, so you can use everything" look identical in a list of names, and a user asking an admin
 *  why a model is missing needs the distinction as much as the admin does.
 */
export default function ModelsPage() {
  const { t } = useTranslation()
  const [data, setData] = useState<AvailableModels | null>(null)
  const [error, setError] = useState('')

  const load = useCallback(() => {
    getAvailableModels()
      .then((d) => {
        setData(d)
        setError('')
      })
      .catch((e) => setError(String(e)))
  }, [])

  useEffect(() => {
    load()
  }, [load])

  if (error) {
    return (
      <div className="panel">
        <div className="panel-body">
          <p className="panel-note" style={{ margin: 0 }}>
            <span className="badge error">{t('common.error')}</span> {error}
          </p>
        </div>
      </div>
    )
  }
  if (!data) return <div className="empty">{t('common.loading')}</div>

  const blocked = data.enabled && !data.unrestricted && data.models.length === 0

  return (
    <>
      {/* -- The effective policy in one line -- */}
      <div className="panel">
        <div className="panel-head">
          {t('models.title')}
          <span className={`badge ${blocked ? 'error' : ''}`}>
            {t('models.count', { count: data.models.length })}
          </span>
          {data.unrestricted ? (
            <span className="badge ok">{t('models.unrestricted')}</span>
          ) : (
            <span className="badge">{t('models.restricted')}</span>
          )}
          <span className="spacer" />
          <button className="btn subtle sm" onClick={load}>
            {t('common.refresh')}
          </button>
        </div>
        <div className="panel-body">
          <p className="panel-note" style={{ margin: 0 }}>
            <Trans
              i18nKey={`models.reason.${data.reason}`}
              components={{ strong: <strong />, code: <code /> }}
            />
          </p>
        </div>
      </div>

      {/* -- The list itself -- */}
      <div className="panel">
        <div className="panel-head">{t('models.listTitle')}</div>
        <div className="panel-body">
          {blocked ? (
            <p className="panel-note" style={{ margin: 0 }}>
              <span className="badge error">{t('models.blockedBadge')}</span>{' '}
              <Trans i18nKey="models.blockedNote" components={{ strong: <strong /> }} />
            </p>
          ) : !data.models.length ? (
            <div className="empty">{t('models.emptyCatalog')}</div>
          ) : (
            <table>
              {/* table-layout is fixed globally, so without this the three columns split evenly:
                  a third of the page for a one-badge Traits cell, and the description -- the only
                  column whose content is a sentence -- squeezed into the same third. */}
              <colgroup>
                <col style={{ width: '28%' }} />
                <col style={{ width: '14%' }} />
                <col style={{ width: '58%' }} />
              </colgroup>
              <thead>
                <tr>
                  <th>{t('models.table.model')}</th>
                  <th>{t('models.table.traits')}</th>
                  <th>{t('models.table.description')}</th>
                </tr>
              </thead>
              <tbody>
                {data.models.map((name) => {
                  const meta = data.catalog[name] ?? {
                    description: '',
                    reasoning: false,
                    default: false,
                  }
                  return (
                    <tr key={name}>
                      <td className="mono">
                        {name}
                        {name === data.default_model && (
                          <>
                            {' '}
                            <span className="badge ok">{t('models.defaultBadge')}</span>
                          </>
                        )}
                      </td>
                      <td>
                        {meta.reasoning ? (
                          <span className="badge warn">{t('models.reasoningBadge')}</span>
                        ) : (
                          <span className="dim">{t('models.generalPurpose')}</span>
                        )}
                      </td>
                      <td className="dim">{meta.description || '-'}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {/* -- Where the list came from. Only the grants that applied are ever returned, so this
             table can never disclose the teams or organizations the user is not in. -- */}
      {data.contributions.length > 0 && (
        <div className="panel">
          <div className="panel-head">
            {t('models.sourceTitle')}
            <span className="badge">
              {t('models.grantCount', { count: data.contributions.length })}
            </span>
          </div>
          <div className="panel-body">
            <p className="panel-note" style={{ marginTop: 0 }}>
              <Trans i18nKey="models.sourceLead" components={{ strong: <strong /> }} />
            </p>
            <table>
              {/* Grants is a joined model list and the widest thing here by far; Scope is one of
                  three fixed words and needs nothing like a quarter of the table. */}
              <colgroup>
                <col style={{ width: '12%' }} />
                <col style={{ width: '24%' }} />
                <col style={{ width: '18%' }} />
                <col style={{ width: '46%' }} />
              </colgroup>
              <thead>
                <tr>
                  <th>{t('models.table.scope')}</th>
                  <th>{t('models.table.name')}</th>
                  <th>{t('models.table.group')}</th>
                  <th>{t('models.table.grants')}</th>
                </tr>
              </thead>
              <tbody>
                {data.contributions.map((c, i) => (
                  <tr key={`${c.scope}-${c.name}-${i}`}>
                    <td>{t(`models.scope.${c.scope}`, { defaultValue: c.scope })}</td>
                    <td className="mono truncate">{c.name}</td>
                    <td className="mono">{c.group}</td>
                    <td className="mono">
                      {c.models.length ? (
                        c.models.join(t('common.listSeparator'))
                      ) : (
                        <span className="dim">{t('models.grantsNothing')}</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </>
  )
}
