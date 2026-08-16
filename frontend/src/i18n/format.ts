/**
 * Locale-aware number and date formatting.
 *
 * These replace the hardcoded locale tags that used to be scattered across the pages
 * (and disagreed with each other -- one file passed 'zh-CN', another 'en-US').
 * Everything goes through the active i18next language now.
 */
import i18next from 'i18next'

import type { Locale } from './index'

/** Our locale ids are not all valid Intl tags, so map them explicitly. */
const INTL_TAG: Record<Locale, string> = {
  en: 'en-US',
  'zh-Hans': 'zh-CN',
  'zh-Hant': 'zh-TW',
  ja: 'ja-JP',
  ko: 'ko-KR',
}

/** The BCP-47 tag for the active language, for Intl and the <html lang> attribute. */
export function localeTag(locale?: string): string {
  const key = (locale ?? i18next.language) as Locale
  return INTL_TAG[key] ?? INTL_TAG.en
}

/** Thousands-separated integer, e.g. 12,345 / 12 345 depending on the locale. */
export function formatInt(value: number | null | undefined): string {
  if (value === null || value === undefined || Number.isNaN(value)) return '-'
  return Math.round(value).toLocaleString(localeTag())
}

/** Date + time, medium length -- used for created_at / last_used_at / trace timestamps. */
export function formatDateTime(value: string | number | Date | null | undefined): string {
  if (!value) return '-'
  const d = value instanceof Date ? value : new Date(value)
  if (Number.isNaN(d.getTime())) return '-'
  return d.toLocaleString(localeTag())
}

/** Date only, for day-granularity axis labels and range pickers. */
export function formatDate(value: string | number | Date | null | undefined): string {
  if (!value) return '-'
  const d = value instanceof Date ? value : new Date(value)
  if (Number.isNaN(d.getTime())) return '-'
  return d.toLocaleDateString(localeTag())
}
