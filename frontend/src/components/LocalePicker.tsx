import { useTranslation } from 'react-i18next'

import { LOCALES, LOCALE_LABELS, type Locale } from '../i18n'
import { useLocale } from '../i18n/useLocale'

/**
 * Language selector. Options carry their **native** names, so someone who cannot read
 * the language currently on screen can still find their own.
 *
 * `compact` is for the pre-sign-in pages (Login / Setup), where there is no topbar to
 * sit in.
 */
export default function LocalePicker({ compact = false }: { compact?: boolean }) {
  const { t } = useTranslation()
  const [locale, setLocale] = useLocale()

  // The wrapper carries the caret. The select itself has `appearance: none`, because the
  // dropdown button the OS draws is sized and coloured by the OS and made this control look
  // unrelated to the status chip beside it in the topbar.
  return (
    <span className={`locale-field ${compact ? 'compact' : ''}`}>
      <select
        className={`locale-picker ${compact ? 'compact' : ''}`}
        value={locale}
        title={t('shell.language')}
        aria-label={t('shell.language')}
        onChange={(e) => setLocale(e.target.value as Locale)}
      >
        {LOCALES.map((l) => (
          <option key={l} value={l}>
            {LOCALE_LABELS[l]}
          </option>
        ))}
      </select>
    </span>
  )
}
