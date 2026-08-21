/** A collapsible, syntax-colored JSON tree.
 *
 *  Recursion is over `typeof` rather than a schema: trace payloads differ by provider, so there
 *  is no fixed shape to render against. `defaultDepth` keeps the first levels open -- a request
 *  body that opens fully collapsed would be strictly worse than the <pre> dump it replaces.
 *
 *  Hand-rolled rather than pulled from npm: the app has five runtime dependencies and a JSON
 *  viewer is not worth being the sixth.
 */
import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { copyText } from '../clipboard'

type Kind = 'string' | 'number' | 'boolean' | 'null' | 'object' | 'array'

function kindOf(value: unknown): Kind {
  if (value === null || value === undefined) return 'null'
  if (Array.isArray(value)) return 'array'
  switch (typeof value) {
    case 'string': return 'string'
    case 'number': return 'number'
    case 'boolean': return 'boolean'
    default: return 'object'
  }
}

function Leaf({ value, kind }: { value: unknown; kind: Kind }) {
  const text =
    kind === 'string' ? JSON.stringify(value)
    : kind === 'null' ? 'null'
    : String(value)
  return <span className={`json-${kind}`}>{text}</span>
}

interface NodeProps {
  label?: string
  value: unknown
  depth: number
  defaultDepth: number
  /** Bumped by expand-all / collapse-all. Every node keys its open state off it, so the buttons
   *  override per-node state instead of only affecting nodes that happen to be mounted. */
  revision: number
  forced: boolean | null
  last: boolean
}

function Node({ label, value, depth, defaultDepth, revision, forced, last }: NodeProps) {
  const kind = kindOf(value)
  const container = kind === 'object' || kind === 'array'
  const [open, setOpen] = useState(depth < defaultDepth)

  // Re-seed from the expand/collapse-all buttons. Keyed on revision, not on `forced`, so pressing
  // the same button twice still re-applies it after the user has toggled nodes by hand.
  useEffect(() => {
    if (forced !== null) setOpen(forced)
  }, [revision, forced])

  const key = label === undefined ? null : <span className="json-key">{JSON.stringify(label)}</span>

  if (!container) {
    return (
      <div className="json-row">
        {key}{key && <span className="json-punct">: </span>}
        <Leaf value={value} kind={kind} />
        {!last && <span className="json-punct">,</span>}
      </div>
    )
  }

  const entries: [string | undefined, unknown][] = kind === 'array'
    ? (value as unknown[]).map((v) => [undefined, v])
    : Object.entries(value as Record<string, unknown>)
  const [openBracket, closeBracket] = kind === 'array' ? ['[', ']'] : ['{', '}']

  return (
    <div className="json-node">
      <div className="json-row">
        <button
          type="button"
          className="json-toggle"
          onClick={() => setOpen(!open)}
          aria-expanded={open}
        >
          {open ? '▾' : '▸'}
        </button>
        {key}{key && <span className="json-punct">: </span>}
        <span className="json-punct">{openBracket}</span>
        {!open && (
          <>
            <span className="json-summary">
              {'…'}{entries.length}
            </span>
            <span className="json-punct">{closeBracket}</span>
            {!last && <span className="json-punct">,</span>}
          </>
        )}
      </div>
      {open && (
        <>
          <div className="json-children">
            {entries.map(([k, v], i) => (
              <Node
                key={k ?? i}
                label={k}
                value={v}
                depth={depth + 1}
                defaultDepth={defaultDepth}
                revision={revision}
                forced={forced}
                last={i === entries.length - 1}
              />
            ))}
          </div>
          <div className="json-row">
            <span className="json-punct">{closeBracket}</span>
            {!last && <span className="json-punct">,</span>}
          </div>
        </>
      )}
    </div>
  )
}

export default function JsonView({
  value,
  defaultDepth = 2,
}: {
  value: unknown
  defaultDepth?: number
}) {
  const { t } = useTranslation()
  const [revision, setRevision] = useState(0)
  const [forced, setForced] = useState<boolean | null>(null)
  const [copied, setCopied] = useState(false)

  const text = useMemo(() => {
    try {
      return JSON.stringify(value, null, 2) ?? ''
    } catch {
      // A payload with a circular reference is not worth crashing the trace detail over.
      return String(value)
    }
  }, [value])

  function force(next: boolean) {
    setForced(next)
    setRevision((r) => r + 1)
  }

  async function copy() {
    // Through the shared helper, which also works on a non-secure origin where
    // navigator.clipboard does not exist at all. A refusal is not worth an error banner here:
    // the JSON is on screen and selectable.
    if (await copyText(text)) {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    }
  }

  return (
    <div className="json-view">
      <div className="json-tools">
        <button type="button" className="btn-link" onClick={() => force(true)}>
          {t('traces.json.expandAll')}
        </button>
        <button type="button" className="btn-link" onClick={() => force(false)}>
          {t('traces.json.collapseAll')}
        </button>
        <button type="button" className="btn-link" onClick={copy}>
          {copied ? t('traces.json.copied') : t('traces.json.copy')}
        </button>
      </div>
      <div className="json-body">
        <Node
          value={value}
          depth={0}
          defaultDepth={defaultDepth}
          revision={revision}
          forced={forced}
          last
        />
      </div>
    </div>
  )
}
