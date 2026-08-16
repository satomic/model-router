import { useEffect, useRef, useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'
import { getDefaultDecisionPrompt, previewDecisionPrompt, type PromptPreview } from '../../api'
import type { SectionProps } from './types'

const PLACEHOLDER = '{catalog}'

/** Module-level, so these hold catalog keys rather than finished sample sentences. */
const SAMPLE_PRESET_KEYS = ['refactor', 'translate', 'proof']

/** The AI decision prompt: edit the template, and preview the prompt actually rendered from
 *  the **current model catalog**.
 *
 *  The preview comes from the backend's `/v1/config/decision-prompt/preview`, which runs the
 *  same rendering function `route_by_ai` uses -- the frontend does not reimplement the
 *  assembly, because the two copies would drift apart sooner or later and the "preview" would
 *  stop meaning anything. The draft (unsaved models / ai_router) travels with the request, so
 *  edited model descriptions show up without having to save first. */
export default function DecisionPromptPanel({ cfg, set, notify, goto }: SectionProps) {
  const { t } = useTranslation()
  const isAi = cfg.strategy === 'ai'
  const prompt = cfg.ai_router.decision_prompt ?? ''
  const [sample, setSample] = useState('')
  const [preview, setPreview] = useState<PromptPreview | null>(null)
  /** The built-in default **template** (placeholder still unreplaced). The rendered preview
   *  cannot serve as the placeholder text -- that is the result, and it would suggest the
   *  template itself contains a long list of models. */
  const [defaultTemplate, setDefaultTemplate] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const taRef = useRef<HTMLTextAreaElement>(null)

  useEffect(() => {
    getDefaultDecisionPrompt()
      .then(({ prompt: text }) => setDefaultTemplate(text))
      .catch(() => setDefaultTemplate(''))
  }, [])

  const setPrompt = (value: string) =>
    // An empty string is stored as undefined: config.yaml keeps no empty field, and the
    // meaning is exactly "use the built-in default"
    set({ ai_router: { ...cfg.ai_router, decision_prompt: value || undefined } })

  // Re-render whenever the model catalog, the prompt or the sample changes. Debounced by
  // 400ms so typing does not hit the endpoint on every keystroke.
  useEffect(() => {
    setLoading(true)
    const timer = setTimeout(() => {
      previewDecisionPrompt({ models: cfg.models, ai_router: cfg.ai_router, sample_prompt: sample })
        .then((p) => {
          setPreview(p)
          setError(null)
        })
        .catch((e) => setError(String(e)))
        .finally(() => setLoading(false))
    }, 400)
    return () => clearTimeout(timer)
  }, [cfg.models, cfg.ai_router, sample])

  const restoreDefault = async () => {
    try {
      const text = defaultTemplate || (await getDefaultDecisionPrompt()).prompt
      setDefaultTemplate(text)
      setPrompt(text)
      notify('ok', t('config.prompt.defaultLoaded'))
    } catch (e) {
      notify('error', String(e))
    }
  }

  /** Insert the placeholder at the caret; append at the end when there is no caret info. */
  const insertPlaceholder = () => {
    const at = taRef.current?.selectionStart ?? prompt.length
    setPrompt(`${prompt.slice(0, at)}${PLACEHOLDER}${prompt.slice(at)}`)
  }

  const missing = preview?.models_without_description ?? []

  return (
    <div className="panel">
      <div className="panel-head">
        {t('config.prompt.title')}
        {!isAi && <span className="badge">{t('config.aiRouter.inactive')}</span>}
        {preview?.is_default_prompt && <span className="badge">{t('config.prompt.usingDefault')}</span>}
      </div>
      <div className="panel-body">
        <p className="panel-note">
          <Trans
            i18nKey="config.prompt.lead"
            values={{ placeholder: PLACEHOLDER, shape: '{"model": ..., "rationale": ...}' }}
            components={{ code: <code /> }}
          />
        </p>

        <label className="field">
          <span className="field-name">
            {t('config.prompt.template')}
            <span className="field-hint">{t('config.prompt.templateHint')}</span>
          </span>
          <textarea
            ref={taRef}
            className="mono prompt-editor"
            rows={12}
            spellCheck={false}
            value={prompt}
            placeholder={defaultTemplate || t('config.prompt.templatePlaceholder')}
            onChange={(e) => setPrompt(e.target.value)}
          />
        </label>

        <div className="prompt-actions">
          <button className="btn ghost sm" onClick={restoreDefault}>
            {t('config.prompt.loadDefault')}
          </button>
          <button className="btn ghost sm" onClick={() => setPrompt('')} disabled={!prompt}>
            {t('config.prompt.clear')}
          </button>
          {prompt && !prompt.includes(PLACEHOLDER) && (
            <button className="btn ghost sm" onClick={insertPlaceholder}>
              {t('config.prompt.insertPlaceholder', { placeholder: PLACEHOLDER })}
            </button>
          )}
          <span className="spacer" />
          {/* Template length != rendered length (the catalog is not substituted yet), so the
              two numbers must be labelled separately */}
          <span className="dim mono">
            {prompt
              ? t('config.prompt.templateChars', { count: prompt.length })
              : t('config.prompt.defaultTemplateChars', { count: defaultTemplate.length })}
          </span>
        </div>

        {prompt.trim() && !prompt.includes(PLACEHOLDER) && (
          <div className="toast warn">
            <Trans
              i18nKey="config.prompt.noPlaceholderWarning"
              values={{ placeholder: PLACEHOLDER }}
              components={{ code: <code /> }}
            />
          </div>
        )}

        {missing.length > 0 && (
          <div className="toast warn">
            <Trans
              i18nKey="config.prompt.missingDescription"
              count={missing.length}
              values={{ models: missing.join(', ') }}
              components={{ code: <code className="mono" /> }}
            />
            <button className="btn subtle sm" onClick={() => goto('models')}>
              {t('config.prompt.goFixModels')}
            </button>
          </div>
        )}

        <div className="preview-head">
          <span className="field-name" style={{ marginBottom: 0 }}>
            {t('config.prompt.previewTitle')}
            <span className="field-hint">{t('config.prompt.previewHint')}</span>
          </span>
          <span className="spacer" />
          {loading && <span className="dim">{t('config.prompt.rendering')}</span>}
        </div>

        {error && <div className="toast error">{t('config.prompt.previewFailed', { error })}</div>}

        {preview && (
          <>
            <div className="preview-meta">
              <span className="badge model">{preview.decision_model}</span>
              <span className="dim">
                {t('config.prompt.metaProvider', { name: preview.decision_provider })}
              </span>
              <span className="dim">·</span>
              <span className="dim">
                {t('config.prompt.metaCandidates', { count: preview.model_count })}
              </span>
              {preview.default_model && (
                <>
                  <span className="dim">·</span>
                  <span className="dim">
                    {t('config.prompt.metaFallback', { name: preview.default_model })}
                  </span>
                </>
              )}
              <span className="spacer" />
              <span className="dim mono">
                {t('config.prompt.metaRenderedChars', { count: preview.chars })}
              </span>
            </div>

            <div className="prompt-preview">
              <div className="msg-role">system</div>
              <pre className="code">{preview.system}</pre>
            </div>

            <label className="field" style={{ marginTop: 14 }}>
              <span className="field-name">
                {t('config.prompt.sample')}
                <span className="field-hint">{t('config.prompt.sampleHint')}</span>
              </span>
              <textarea
                rows={2}
                value={sample}
                placeholder={t('config.prompt.samplePlaceholder')}
                onChange={(e) => setSample(e.target.value)}
              />
            </label>
            <div className="prompt-actions" style={{ marginTop: 0 }}>
              {SAMPLE_PRESET_KEYS.map((key) => {
                const text = t(`config.prompt.preset.${key}`)
                return (
                  <button
                    key={key}
                    className="btn subtle sm"
                    title={text}
                    onClick={() => setSample(text)}
                  >
                    {text.length > 16 ? `${text.slice(0, 16)}…` : text}
                  </button>
                )
              })}
            </div>

            {preview.user && (
              <div className="prompt-preview">
                <div className="msg-role">
                  user
                  {preview.sample_truncated && (
                    <span className="badge warn">
                      {t('config.prompt.sampleTruncated', {
                        count: cfg.ai_router.max_prompt_chars,
                      })}
                    </span>
                  )}
                </div>
                <pre className="code">{preview.user}</pre>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  )
}
