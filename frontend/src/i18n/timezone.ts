/**
 * The reader's own time zone, applied to every timestamp the console renders.
 *
 * A per-person preference rather than configuration: it lives in localStorage beside the
 * theme and the locale, so changing it moves nobody else's clocks. Timestamps still travel
 * as UTC on the wire -- only the rendering moves.
 */
export const TZ_STORAGE_KEY = 'timezone'

/** The empty string means "follow the browser", which is what an unset preference gets. */
export type TimeZone = string

function valid(id: string): boolean {
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: id })
    return true
  } catch {
    return false
  }
}

// Validated on load: a stored zone the runtime no longer knows would otherwise make every
// date on the page throw.
let current: TimeZone = (() => {
  const stored = localStorage.getItem(TZ_STORAGE_KEY) ?? ''
  if (stored && !valid(stored)) {
    localStorage.removeItem(TZ_STORAGE_KEY)
    return ''
  }
  return stored
})()

export function timeZone(): TimeZone {
  return current
}

/** What to hand Intl: undefined lets it use the host's zone. */
export function intlTimeZone(): string | undefined {
  return current || undefined
}

export function storeTimeZone(next: TimeZone) {
  current = valid(next) ? next : ''
  if (current) localStorage.setItem(TZ_STORAGE_KEY, current)
  else localStorage.removeItem(TZ_STORAGE_KEY)
}

/** The host's zone, used as the label of the "follow the browser" option. */
export function systemTimeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
}

/** The zone actually in force, i.e. the preference or the host's. */
export function effectiveTimeZone(): string {
  return current || systemTimeZone()
}

/** "UTC+08:00" for a zone *at this instant*, so a zone on summer time reads correctly today. */
export function zoneOffset(id: string): string {
  try {
    const parts = new Intl.DateTimeFormat('en-US', {
      timeZone: id,
      timeZoneName: 'longOffset',
    }).formatToParts(new Date())
    const name = parts.find((p) => p.type === 'timeZoneName')?.value ?? ''
    // Intl says "GMT+08:00", and bare "GMT" for zero.
    return name === 'GMT' ? 'UTC+00:00' : name.replace('GMT', 'UTC')
  } catch {
    return ''
  }
}

/** A short list for runtimes that cannot enumerate zones, covering the common offsets. */
const FALLBACK_ZONES = [
  'UTC', 'Pacific/Honolulu', 'America/Anchorage', 'America/Los_Angeles', 'America/Denver',
  'America/Chicago', 'America/New_York', 'America/Sao_Paulo', 'Europe/London', 'Europe/Berlin',
  'Europe/Moscow', 'Asia/Dubai', 'Asia/Kolkata', 'Asia/Bangkok', 'Asia/Shanghai',
  'Asia/Tokyo', 'Asia/Seoul', 'Australia/Sydney', 'Pacific/Auckland',
]

let cachedIds: string[] | null = null

/** Every zone the runtime knows, sorted west to east so the list reads like a map. */
export function zoneIds(): string[] {
  if (cachedIds) return cachedIds
  const withOf = Intl as typeof Intl & { supportedValuesOf?: (k: string) => string[] }
  const ids = withOf.supportedValuesOf?.('timeZone') ?? FALLBACK_ZONES
  // UTC is unioned in explicitly: the runtime's list omits it, and it is the zone every trace
  // is actually recorded in -- the one choice a reader of this console is most likely to want.
  const all = ids.includes('UTC') ? [...ids] : ['UTC', ...ids]
  cachedIds = all.sort((a, b) => {
    const d = offsetMinutes(a) - offsetMinutes(b)
    return d !== 0 ? d : a.localeCompare(b)
  })
  return cachedIds
}

/** Minutes east of UTC, parsed back out of the formatted offset. */
function offsetMinutes(id: string): number {
  const m = /^UTC([+-])(\d{2}):(\d{2})$/.exec(zoneOffset(id))
  if (!m) return 0
  const mins = Number(m[2]) * 60 + Number(m[3])
  return m[1] === '-' ? -mins : mins
}
