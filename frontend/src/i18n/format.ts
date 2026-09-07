/**
 * Locale-aware number and date formatting.
 *
 * These replace the hardcoded locale tags that used to be scattered across the pages
 * (and disagreed with each other -- one file passed 'zh-CN', another 'en-US').
 * Everything goes through the active i18next language now.
 */
import i18next from 'i18next'

import type { Locale } from './index'
import { intlTimeZone } from './timezone'

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
  const d = toDate(value)
  if (!d) return '-'
  return d.toLocaleString(localeTag(), { timeZone: intlTimeZone() })
}

/** Date only, for day-granularity axis labels and range pickers. */
export function formatDate(value: string | number | Date | null | undefined): string {
  const d = toDate(value)
  if (!d) return '-'
  return d.toLocaleDateString(localeTag(), { timeZone: intlTimeZone() })
}

function toDate(value: string | number | Date | null | undefined): Date | null {
  if (!value) return null
  const d = value instanceof Date ? value : new Date(value)
  return Number.isNaN(d.getTime()) ? null : d
}

/** Sortable `2026-09-07 14:30:05` in the reader's zone.
 *
 *  Deliberately not locale-shaped: this is the form used in dense table cells and beside ids,
 *  where a fixed width and an unambiguous field order matter more than local convention. */
export function formatStamp(value: string | number | Date | null | undefined): string {
  const d = toDate(value)
  if (!d) return '—'
  const p = stampParts(d)
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}`
}

/** Just the clock, for rows already dated by the record around them. */
export function formatClock(value: string | number | Date | null | undefined): string {
  const d = toDate(value)
  if (!d) return '—'
  const p = stampParts(d)
  return `${p.hour}:${p.minute}:${p.second}`
}

function stampParts(d: Date): Record<string, string> {
  // hourCycle h23 rather than hour12:false, which yields "24" for midnight in some locales.
  const parts = new Intl.DateTimeFormat('en-GB', {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
    hourCycle: 'h23',
    timeZone: intlTimeZone(),
  }).formatToParts(d)
  return Object.fromEntries(parts.map((p) => [p.type, p.value]))
}
