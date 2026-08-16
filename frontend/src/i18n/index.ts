/**
 * i18next setup. English is the source language: en.json is authored first and acts as
 * the fallback, the other catalogs are translations of it.
 *
 * No language-detector package is used -- detection is the ten lines in
 * `initialLocale()` below, mirroring the `useTheme` idiom already in App.tsx
 * (localStorage key + an attribute on <html>).
 */
import i18next from 'i18next'
import { initReactI18next } from 'react-i18next'

import en from './locales/en.json'
import ja from './locales/ja.json'
import ko from './locales/ko.json'
import zhHans from './locales/zh-Hans.json'
import zhHant from './locales/zh-Hant.json'

export const LOCALES = ['en', 'zh-Hans', 'zh-Hant', 'ja', 'ko'] as const
export type Locale = (typeof LOCALES)[number]

/** Native names, so a user who cannot read the current language still finds their own. */
export const LOCALE_LABELS: Record<Locale, string> = {
  en: 'English',
  'zh-Hans': '简体中文',
  'zh-Hant': '繁體中文',
  ja: '日本語',
  ko: '한국어',
}

export const LOCALE_STORAGE_KEY = 'locale'

/**
 * Map any browser language tag onto one of our locales.
 *
 * Script matters more than region for Chinese: zh-TW / zh-HK / zh-MO are Traditional,
 * plain `zh` and everything else Simplified. Checking `zh-Hant` alone would miss
 * `zh-TW`, which is what browsers actually send.
 */
export function matchLocale(tag: string): Locale | null {
  const lower = tag.toLowerCase()
  if (lower.startsWith('zh')) {
    if (/hant|tw|hk|mo/.test(lower)) return 'zh-Hant'
    return 'zh-Hans'
  }
  if (lower.startsWith('ja')) return 'ja'
  if (lower.startsWith('ko')) return 'ko'
  if (lower.startsWith('en')) return 'en'
  return null
}

function initialLocale(): Locale {
  const saved = localStorage.getItem(LOCALE_STORAGE_KEY)
  if (saved && (LOCALES as readonly string[]).includes(saved)) return saved as Locale
  for (const tag of navigator.languages ?? [navigator.language]) {
    const hit = matchLocale(tag || '')
    if (hit) return hit
  }
  return 'en'
}

void i18next.use(initReactI18next).init({
  resources: {
    en: { translation: en },
    'zh-Hans': { translation: zhHans },
    'zh-Hant': { translation: zhHant },
    ja: { translation: ja },
    ko: { translation: ko },
  },
  lng: initialLocale(),
  fallbackLng: 'en',
  supportedLngs: LOCALES as unknown as string[],
  defaultNS: 'translation',
  ns: ['translation'],
  // Keys are dotted IDs like `nav.usage`; without this i18next would read `nav` as a
  // namespace prefix and look for a namespace that does not exist.
  nsSeparator: false,
  interpolation: { escapeValue: false }, // React escapes for us
  returnEmptyString: false,
})

export default i18next
