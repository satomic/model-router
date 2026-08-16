import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { sendChat, type ChatResult } from '../api'

const KEY_STORAGE = 'playground_api_key'

export default function PlaygroundPage({ onOpenTrace }: { onOpenTrace: (id: string) => void }) {
  const { t, i18n } = useTranslation()
  const [prompt, setPrompt] = useState(() => t('playground.samplePrompt'))
  const [apiKey, setApiKey] = useState(() => localStorage.getItem(KEY_STORAGE) ?? '')
  const [remember, setRemember] = useState(() => !!localStorage.getItem(KEY_STORAGE))
  const [session, setSession] = useState('')
  const [maxTokens, setMaxTokens] = useState(600)
  const [stream, setStream] = useState(false)
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<ChatResult | null>(null)
  const [error, setError] = useState('')

  // The prompt is the one form field pre-filled from a catalog. Re-translate it when the
  // language changes, but only while the user has not typed anything of their own --
  // silently discarding someone's draft on a language switch would be worse than leaving
  // it in the previous language.
  const promptEdited = useRef(false)
  useEffect(() => {
    if (!promptEdited.current) setPrompt(t('playground.samplePrompt'))
  }, [i18n.language, t])

  useEffect(() => {
    if (remember && apiKey) localStorage.setItem(KEY_STORAGE, apiKey)
    else localStorage.removeItem(KEY_STORAGE)
  }, [remember, apiKey])

  const send = async () => {
    setBusy(true)
    setError('')
    setResult(null)
    try {
      setResult(
        await sendChat({
          prompt,
          apiKey: apiKey.trim(),
          session: session || undefined,
          maxTokens,
          stream,
        }),
      )
    } catch (e) {
      setError(String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="layout-split">
      <div className="panel" style={{ marginBottom: 0 }}>
        <div className="panel-head">{t('playground.sendRequest')}</div>
        <div className="panel-body">
          <label className="field">
            <span className="field-name">
              {t('playground.apiKey')}
              <span className="field-hint">{t('playground.apiKeyHint')}</span>
            </span>
            <input
              type="password"
              className="mono"
              value={apiKey}
              placeholder="mr_..."
              onChange={(e) => setApiKey(e.target.value)}
            />
          </label>
          <label className="check" style={{ marginBottom: 14 }}>
            <input
              type="checkbox"
              checked={remember}
              onChange={(e) => setRemember(e.target.checked)}
            />
            {t('playground.remember')}
          </label>

          <label className="field">
            <span className="field-name">Prompt</span>
            <textarea
              rows={6}
              value={prompt}
              onChange={(e) => {
                promptEdited.current = true
                setPrompt(e.target.value)
              }}
            />
          </label>
          <div className="row">
            <label className="field">
              <span className="field-name">{t('playground.sessionId')}</span>
              <input
                type="text"
                value={session}
                placeholder={t('playground.sessionIdPlaceholder')}
                onChange={(e) => setSession(e.target.value)}
              />
            </label>
            <label className="field">
              <span className="field-name">max_tokens</span>
              <input type="number" value={maxTokens} onChange={(e) => setMaxTokens(+e.target.value)} />
            </label>
          </div>
          <label className="check" style={{ marginBottom: 14 }}>
            <input type="checkbox" checked={stream} onChange={(e) => setStream(e.target.checked)} />
            {t('playground.stream')}
          </label>
          <div>
            <button className="btn" onClick={send} disabled={busy || !prompt.trim() || !apiKey.trim()}>
              {busy ? t('playground.sending') : t('playground.send')}
            </button>
          </div>
        </div>
      </div>

      <div className="panel" style={{ marginBottom: 0 }}>
        <div className="panel-head">
          {t('playground.routingResult')}
          {result && (
            <>
              <span className="spacer" />
              <span className="badge model">{result.model}</span>
              <span className="badge warn">{result.reason}</span>
              <span className="badge">{t('playground.decisionMs', { ms: result.decisionMs })}</span>
            </>
          )}
        </div>
        <div className="panel-body">
          {error && <div className="toast error">{error}</div>}
          {busy && <div className="empty">{t('playground.waiting')}</div>}
          {result && (
            <>
              <div className="reply-box">{result.content || t('playground.emptyContent')}</div>
              <div style={{ marginTop: 14 }}>
                <button className="btn ghost sm" onClick={() => onOpenTrace(result.traceId)}>
                  {t('playground.viewTrace')} · {result.traceId}
                </button>
              </div>
            </>
          )}
          {!busy && !result && !error && <div className="empty">{t('playground.idle')}</div>}
        </div>
      </div>
    </div>
  )
}
