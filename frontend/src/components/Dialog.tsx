import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { useTranslation } from 'react-i18next'

/** Ask-the-user dialogs, in the console's own visual language.
 *
 *  These replace window.confirm / window.prompt / window.alert everywhere. The native dialogs are
 *  drawn by the browser chrome, so they carry none of the Fluent tokens the rest of the console is
 *  built from, cannot be translated (their buttons follow the browser's locale, not the console's),
 *  and block the whole page synchronously.
 *
 *  Two things the native calls could not do at all, and which the call sites here rely on:
 *  - `validate` keeps the dialog open with the error shown in place, so a malformed team key is
 *    corrected where it was typed instead of being rejected by a toast after the fact.
 *  - `danger` marks a destructive confirmation, which is what makes "delete" read differently from
 *    "discard my draft".
 */

interface Common {
  title: string
  /** The explanatory line under the title. A node rather than a string so a call site can bold the
   *  name of the thing being deleted. */
  message?: ReactNode
  confirmLabel?: string
  cancelLabel?: string
}

interface ConfirmRequest extends Common {
  kind: 'confirm'
  /** Renders the primary button as destructive. */
  danger?: boolean
}

interface PromptRequest extends Common {
  kind: 'prompt'
  /** The name shown above the input. Falls back to the title when omitted. */
  label?: string
  defaultValue?: string
  placeholder?: string
  /** Return an error message to keep the dialog open, or null/'' to accept. Runs on submit. */
  validate?: (value: string) => string | null | undefined
  /** Renders the value in the monospace face -- right for ids, logins and group names. */
  mono?: boolean
}

interface AlertRequest extends Common {
  kind: 'alert'
}

type Request = ConfirmRequest | PromptRequest | AlertRequest

/** What a dialog settles to: a boolean for confirm, the trimmed text (or null when cancelled) for
 *  prompt, undefined for alert. */
type Answer = boolean | string | null | undefined

interface Pending {
  req: Request
  resolve: (answer: Answer) => void
}

export interface Dialogs {
  /** true = the user confirmed. */
  confirm: (req: Omit<ConfirmRequest, 'kind'>) => Promise<boolean>
  /** The trimmed input, or null when cancelled. Never returns an empty string: an empty prompt is
   *  a cancellation, which is what every call site already treated it as. */
  prompt: (req: Omit<PromptRequest, 'kind'>) => Promise<string | null>
  alert: (req: Omit<AlertRequest, 'kind'>) => Promise<void>
}

const DialogContext = createContext<Dialogs | null>(null)

/** The dialogs handle. Throws rather than degrading to window.confirm when the provider is
 *  missing: a silent fallback would put a browser dialog back on screen in exactly the case this
 *  component exists to remove, and only ever in some paths, which is worse than a loud failure. */
export function useDialogs(): Dialogs {
  const ctx = useContext(DialogContext)
  if (!ctx) throw new Error('useDialogs must be used inside <DialogProvider>')
  return ctx
}

export function DialogProvider({ children }: { children: ReactNode }) {
  // A queue rather than a single slot: two dialogs can legitimately be asked for in one turn (a
  // batch delete that reports its count afterwards, for instance), and dropping the second or
  // replacing the first would lose an answer a caller is awaiting.
  const [queue, setQueue] = useState<Pending[]>([])

  const enqueue = useCallback((req: Request) => {
    return new Promise<Answer>((resolve) => {
      setQueue((q) => [...q, { req, resolve }])
    })
  }, [])

  const api = useMemo<Dialogs>(
    () => ({
      confirm: (req) => enqueue({ ...req, kind: 'confirm' }) as Promise<boolean>,
      prompt: (req) => enqueue({ ...req, kind: 'prompt' }) as Promise<string | null>,
      alert: (req) => enqueue({ ...req, kind: 'alert' }).then(() => undefined),
    }),
    [enqueue],
  )

  const current = queue[0]

  const settle = useCallback((answer: Answer) => {
    setQueue((q) => {
      const [head, ...rest] = q
      head?.resolve(answer)
      return rest
    })
  }, [])

  return (
    <DialogContext.Provider value={api}>
      {children}
      {current && (
        // Keyed on the queue length so the next dialog in line mounts fresh -- otherwise a prompt
        // following a prompt would inherit the previous one's typed value and error.
        <DialogHost key={queue.length} req={current.req} onSettle={settle} />
      )}
    </DialogContext.Provider>
  )
}

function DialogHost({ req, onSettle }: { req: Request; onSettle: (answer: Answer) => void }) {
  const { t } = useTranslation()
  const [value, setValue] = useState(req.kind === 'prompt' ? (req.defaultValue ?? '') : '')
  const [error, setError] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)
  const okRef = useRef<HTMLButtonElement>(null)

  const cancelAnswer: Answer = req.kind === 'confirm' ? false : req.kind === 'prompt' ? null : undefined

  // Focus on open: the input for a prompt, the primary button otherwise. Without this the focus
  // stays on whatever opened the dialog, so Enter would re-trigger that button instead of the
  // dialog's, and a keyboard user would have to tab into the dialog.
  useEffect(() => {
    const el = req.kind === 'prompt' ? inputRef.current : okRef.current
    el?.focus()
    if (req.kind === 'prompt') inputRef.current?.select()
  }, [req.kind])

  // Escape cancels from anywhere in the dialog, matching both the native dialogs and Fluent.
  // Bound on the document rather than the panel so it works before the first tab press, and
  // captured so a page-level Escape handler cannot swallow it first.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.stopPropagation()
      onSettle(cancelAnswer)
    }
    document.addEventListener('keydown', onKey, true)
    return () => document.removeEventListener('keydown', onKey, true)
  }, [cancelAnswer, onSettle])

  // The page behind must not scroll while a modal is up: a wheel event over the overlay would
  // otherwise move the content the dialog is asking about.
  useEffect(() => {
    const previous = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.body.style.overflow = previous
    }
  }, [])

  const submit = () => {
    if (req.kind !== 'prompt') {
      onSettle(req.kind === 'confirm' ? true : undefined)
      return
    }
    const trimmed = value.trim()
    // An empty prompt is a cancellation, which is how every call site already read `prompt()`
    // returning ''. Reported in place rather than silently, so an accidental Enter says why.
    if (!trimmed) {
      setError(t('dialog.valueRequired'))
      inputRef.current?.focus()
      return
    }
    const problem = req.validate?.(trimmed)
    if (problem) {
      setError(problem)
      inputRef.current?.focus()
      return
    }
    onSettle(trimmed)
  }

  const danger = req.kind === 'confirm' && req.danger
  // A prompt accepts with the same neutral "OK" as an alert: the naming dialogs it backs are
  // edits to a draft, not saves, so a "Save" label would promise persistence the button does not
  // deliver. Only a confirm gets a verb, and a destructive one is labelled by its call site.
  const confirmLabel =
    req.confirmLabel ?? (req.kind === 'confirm' ? t('dialog.confirm') : t('dialog.ok'))

  return (
    <div
      className="dialog-overlay"
      // A click on the backdrop cancels, the same as Escape. Guarded on the target so a drag that
      // starts inside the panel and ends on the backdrop does not dismiss it.
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onSettle(cancelAnswer)
      }}
    >
      <div
        className={`dialog ${danger ? 'danger' : ''}`}
        role={req.kind === 'alert' ? 'alertdialog' : 'dialog'}
        aria-modal="true"
        aria-labelledby="dialog-title"
      >
        <div className="dialog-head">
          <h2 id="dialog-title">{req.title}</h2>
        </div>
        <div className="dialog-body">
          {req.message && <div className="dialog-message">{req.message}</div>}
          {req.kind === 'prompt' && (
            <label className="field" style={{ marginBottom: 0, marginTop: req.message ? 12 : 0 }}>
              {req.label && <span className="field-name">{req.label}</span>}
              <input
                ref={inputRef}
                type="text"
                className={req.mono ? 'mono' : undefined}
                value={value}
                placeholder={req.placeholder}
                onChange={(e) => {
                  setValue(e.target.value)
                  setError('')
                }}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') {
                    e.preventDefault()
                    submit()
                  }
                }}
              />
              {error && <span className="dialog-error">{error}</span>}
            </label>
          )}
        </div>
        <div className="dialog-foot">
          {req.kind !== 'alert' && (
            <button className="btn ghost" onClick={() => onSettle(cancelAnswer)}>
              {req.cancelLabel ?? t('dialog.cancel')}
            </button>
          )}
          <button ref={okRef} className={`btn ${danger ? 'destructive' : ''}`} onClick={submit}>
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}
