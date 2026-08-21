import { useState } from 'react'
import { Trans, useTranslation } from 'react-i18next'

/** What to put where the key belongs when its value is not available on this page. */
const PLACEHOLDER = 'YOUR_API_KEY'

/** Command-line usage for one key, in either protocol.
 *
 *  Shared by the panel shown straight after a key is created and by the key list, because those
 *  two showed different things for no reason: a user who closed the created-key panel, or came
 *  back the next day, had nowhere to read the base URL, the header names or the curl line again.
 *  One component means the list cannot drift from what the create panel promised.
 *
 *  Both protocols are offered on the same key on purpose. `/v1/chat/completions` and
 *  `/v1/messages` both reach every model, whichever series it belongs to, and the router converts
 *  between the two wire formats -- so which one to show is the client's business, not the key's.
 */
export default function CliExamples({
  keyValue,
  login,
  missing,
  copy,
  copiedId,
  copyId,
}: {
  /** The plaintext key, or null when this page may not show it. */
  keyValue: string | null
  /** The owner, for the attribution note: Copilot BYOK sends no user id, so the key is the
   *  identity and every request lands under this account. */
  login: string
  /** Why there is no plaintext, when there is none. Picks the note that explains the placeholder. */
  missing?: 'otherUser' | 'unavailable'
  copy: (id: string, text: string) => void
  copiedId: string | null
  /** Namespaces the copy confirmation, so it appears on the snippet that was copied and not on
   *  every one of them at once. */
  copyId: string
}) {
  const { t } = useTranslation()
  const [mode, setMode] = useState<'openai' | 'anthropic'>('openai')

  const origin = window.location.origin
  const secret = keyValue ?? PLACEHOLDER

  // The one-line form is what the button copies. Kept next to the block below so the two cannot
  // describe different requests: a copied command that does not match what was on screen is worse
  // than no copy button.
  const command =
    mode === 'openai'
      ? `curl ${origin}/v1/chat/completions -H "Authorization: Bearer ${secret}" -H "Content-Type: application/json" -d '{"messages":[{"role":"user","content":"hello"}]}'`
      : `curl ${origin}/v1/messages -H "x-api-key: ${secret}" -H "anthropic-version: 2023-06-01" -H "Content-Type: application/json" -d '{"model":"auto","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'`

  const block =
    mode === 'openai'
      ? `OPENAI_BASE_URL : ${origin}/v1
OPENAI_API_KEY  : ${secret}
Model           : ${t('keys.cli.modelHint')}

# ${t('keys.cli.curlComment')}
curl ${origin}/v1/chat/completions \\
  -H "Authorization: Bearer ${secret}" \\
  -H "Content-Type: application/json" \\
  -d '{"messages":[{"role":"user","content":"hello"}]}'`
      : `ANTHROPIC_BASE_URL   : ${origin}
ANTHROPIC_AUTH_TOKEN : ${secret}
ANTHROPIC_MODEL      : ${t('keys.cli.modelHint')}

# ${t('keys.cli.curlComment')}
curl ${origin}/v1/messages \\
  -H "x-api-key: ${secret}" \\
  -H "anthropic-version: 2023-06-01" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"auto","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'

# ${t('keys.cli.modelIgnored')}`

  return (
    <div>
      <div className="field-name">
        {t('keys.cli.byokTitle')}
        <span className="field-hint">{t('keys.cli.protocolHint')}</span>
      </div>
      <div className="scope-kinds" style={{ marginBottom: 8 }}>
        {(['openai', 'anthropic'] as const).map((m) => (
          <label className="check" key={m}>
            <input
              type="radio"
              name={`cli-${copyId}`}
              checked={mode === m}
              onChange={() => setMode(m)}
            />
            <span>{t(`keys.cli.protocol.${m}`)}</span>
          </label>
        ))}
      </div>

      {/* Said before the snippet rather than after it: somebody who reads only the block would
          otherwise paste a placeholder into a client config and get a 401 back. */}
      {!keyValue && (
        <p className="panel-note" style={{ marginTop: 0 }}>
          <span className="badge warn">{t('keys.cli.placeholderBadge')}</span>
          <span> </span>
          <Trans
            i18nKey={missing === 'otherUser' ? 'keys.cli.otherUserNote' : 'keys.cli.unavailableNote'}
            values={{ login, placeholder: PLACEHOLDER }}
            components={{ code: <code /> }}
          />
        </p>
      )}

      <pre className="code">{block}</pre>

      <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
        <button className="btn ghost sm" onClick={() => copy(copyId, command)}>
          {copiedId === copyId ? t('common.copied') : t('keys.cli.copyCommand')}
        </button>
      </div>

      <p className="panel-note" style={{ marginTop: 10, marginBottom: 0 }}>
        <Trans i18nKey="keys.cli.attribution" values={{ login }} components={{ code: <code /> }} />
      </p>
    </div>
  )
}
