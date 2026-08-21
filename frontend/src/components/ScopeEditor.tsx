import { useTranslation } from 'react-i18next'
import { API_TYPES, type ApiType, type KeyScope } from '../api'

/** One row of the model list the editor picks from: the name plus the connection type behind it. */
export interface ScopeModel {
  name: string
  api_type: string
}

/** The three scope kinds in the order they are offered, narrowest last. */
const KINDS: KeyScope['kind'][] = ['all', 'api_types', 'models']

/**
 * Edit what a single API key may reach.
 *
 * A scope only ever subtracts from what its owner is allowed (see app/keyscope.py), so this
 * editor never has to reason about permission: everything it offers is already something the
 * caller can use, and the backend re-checks the result against the key's owner anyway.
 *
 * The "connection type" kind is stored as the type, not as the models it currently resolves to,
 * which is why the count beside each type is shown as information rather than as the selection.
 * A model added to that connection next week is covered by this key without anyone editing it.
 */
export default function ScopeEditor({
  value,
  onChange,
  models,
  disabled,
}: {
  value: KeyScope
  onChange: (next: KeyScope) => void
  models: ScopeModel[]
  disabled?: boolean
}) {
  const { t } = useTranslation()
  const selectedTypes = value.kind === 'api_types' ? value.api_types : []
  const selectedModels = value.kind === 'models' ? value.models : []

  const pickKind = (kind: KeyScope['kind']) => {
    if (kind === 'all') return onChange({ kind: 'all' })
    if (kind === 'api_types') {
      // Seeded from the types the caller's own models actually sit on, so switching to this kind
      // lands on a selection that covers something instead of an empty one the backend refuses.
      const present = API_TYPES.filter((tp) => models.some((m) => m.api_type === tp))
      return onChange({ kind: 'api_types', api_types: selectedTypes.length ? selectedTypes : present.slice(0, 1) })
    }
    onChange({ kind: 'models', models: selectedModels })
  }

  const toggleType = (tp: ApiType) => {
    const next = selectedTypes.includes(tp)
      ? selectedTypes.filter((x) => x !== tp)
      : [...selectedTypes, tp]
    onChange({ kind: 'api_types', api_types: next })
  }

  const toggleModel = (name: string) => {
    const next = selectedModels.includes(name)
      ? selectedModels.filter((x) => x !== name)
      : [...selectedModels, name]
    onChange({ kind: 'models', models: next })
  }

  return (
    <div className="scope-editor">
      <div className="scope-kinds">
        {KINDS.map((kind) => (
          <label className="check" key={kind}>
            <input
              type="radio"
              checked={(value.kind ?? 'all') === kind}
              disabled={disabled}
              onChange={() => pickKind(kind)}
            />
            <span>
              {t(`keys.scope.kind.${kind}`)}
              <span className="field-hint">{t(`keys.scope.kindHint.${kind}`)}</span>
            </span>
          </label>
        ))}
      </div>

      {value.kind === 'api_types' && (
        <div className="scope-choices">
          {API_TYPES.map((tp) => {
            const covered = models.filter((m) => m.api_type === tp).length
            return (
              <label className="check" key={tp}>
                <input
                  type="checkbox"
                  checked={selectedTypes.includes(tp)}
                  disabled={disabled}
                  onChange={() => toggleType(tp)}
                />
                <span>
                  <code className="mono">{tp}</code>{' '}
                  <span className="dim">
                    {covered
                      ? t('keys.scope.typeCovers', { count: covered })
                      : t('keys.scope.typeCoversNone')}
                  </span>
                </span>
              </label>
            )
          })}
          {selectedTypes.length === 0 && (
            <p className="panel-note scope-warn">{t('keys.scope.pickAType')}</p>
          )}
        </div>
      )}

      {value.kind === 'models' && (
        <div className="scope-choices">
          {models.length === 0 ? (
            <p className="panel-note scope-warn">{t('keys.scope.noModels')}</p>
          ) : (
            <>
              <div className="scope-bulk">
                <button
                  type="button"
                  className="btn-link"
                  disabled={disabled}
                  onClick={() => onChange({ kind: 'models', models: models.map((m) => m.name) })}
                >
                  {t('keys.scope.selectAll')}
                </button>
                <button
                  type="button"
                  className="btn-link"
                  disabled={disabled}
                  onClick={() => onChange({ kind: 'models', models: [] })}
                >
                  {t('keys.scope.clear')}
                </button>
              </div>
              {models.map((m) => (
                <label className="check" key={m.name}>
                  <input
                    type="checkbox"
                    checked={selectedModels.includes(m.name)}
                    disabled={disabled}
                    onChange={() => toggleModel(m.name)}
                  />
                  <span>
                    <code className="mono">{m.name}</code>
                    {m.api_type && <span className="badge">{m.api_type}</span>}
                  </span>
                </label>
              ))}
              {selectedModels.length === 0 && (
                <p className="panel-note scope-warn">{t('keys.scope.pickAModel')}</p>
              )}
            </>
          )}
        </div>
      )}
    </div>
  )
}

/** True when the scope is complete enough for the backend to accept it, so a Create or Save
 *  button can refuse locally instead of round-tripping to a 400. */
export function scopeIsComplete(scope: KeyScope): boolean {
  if (scope.kind === 'api_types') return scope.api_types.length > 0
  if (scope.kind === 'models') return scope.models.length > 0
  return true
}

/** Compare two scopes by value, so an edit form can tell whether anything actually changed. */
export function sameScope(a: KeyScope | undefined, b: KeyScope | undefined): boolean {
  const key = (s: KeyScope | undefined) => {
    const scope = s ?? { kind: 'all' as const }
    if (scope.kind === 'api_types') return `api_types:${[...scope.api_types].sort().join(',')}`
    if (scope.kind === 'models') return `models:${[...scope.models].sort().join(',')}`
    return 'all'
  }
  return key(a) === key(b)
}
