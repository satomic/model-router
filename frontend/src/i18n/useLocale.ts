/**
 * The write side of the locale, shaped like the `useTheme` hook in App.tsx: read the
 * current value, set a new one, persist it, and reflect it on <html>.
 *
 * The read side needs no hook and no React context -- `useTranslation()` subscribes to
 * the i18next instance directly, so every component re-renders on a language change
 * without anything being threaded through props.
 */
import { useCallback, useEffect } from 'react'
import { useTranslation } from 'react-i18next'

import { LOCALE_STORAGE_KEY, type Locale } from './index'
import { localeTag } from './format'

export function useLocale(): [Locale, (next: Locale) => void] {
  const { i18n } = useTranslation()
  const current = i18n.language as Locale

  // Screen readers and the browser's own font selection read this attribute; CJK in
  // particular picks the wrong glyph variants under lang="en". Runs on mount too, so
  // the static lang="en" in index.html is corrected for a returning user.
  useEffect(() => {
    document.documentElement.setAttribute('lang', localeTag(current))
  }, [current])

  const setLocale = useCallback(
    (next: Locale) => {
      void i18n.changeLanguage(next)
      localStorage.setItem(LOCALE_STORAGE_KEY, next)
    },
    [i18n],
  )

  return [current, setLocale]
}
