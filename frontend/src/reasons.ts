/**
 * The two policy verdicts on the API keys page, said in the reader's language.
 *
 * app/keypolicy.py and app/scopepolicy.py each return one English sentence plus a
 * machine-readable `reason_code` and the parameters that sentence interpolates. The sentence is
 * the record: it goes into the server log and into the 403 body, where the reader's locale is
 * unknown and must not change what was written down. The console shows the same verdict from the
 * catalogs instead, and falls back to the server's sentence for a code it does not know, so an
 * older console against a newer server degrades to English rather than to a missing key.
 */
import type { TFunction } from 'i18next'

import type { AccessVerdict, KeyScopeVerdict } from './api'
import { localeTag } from './i18n/format'

/** Intl.ListFormat is ES2021 and this project compiles against the ES2020 lib, so it is typed
 *  here rather than by widening the whole compilation for one call. */
type ListFormatCtor = new (
  locales?: string,
  options?: { style?: 'long' | 'short' | 'narrow'; type?: 'conjunction' | 'disjunction' | 'unit' },
) => { format(items: string[]): string }

/** "user and team", "用户和团队": neither the separator nor the conjunction is a comma
 *  everywhere, so the joining is Intl's job rather than a string in five catalogs. */
function joinNames(names: string[]): string {
  const ctor = (Intl as unknown as { ListFormat?: ListFormatCtor }).ListFormat
  // Absent on older engines, and a comma list still reads correctly there.
  if (!ctor) return names.join(', ')
  try {
    return new ctor(localeTag(), { style: 'long', type: 'conjunction' }).format(names)
  } catch {
    return names.join(', ')
  }
}

/** Why this account may or may not create an API key. */
export function keyPolicyReason(t: TFunction, verdict: AccessVerdict | null): string {
  if (!verdict) return ''
  if (!verdict.reason_code) return verdict.reason
  return t(`keys.access.reason.${verdict.reason_code}`, {
    ...(verdict.reason_params ?? {}),
    defaultValue: verdict.reason,
  })
}

/** Why this account may or may not narrow a key's scope. */
export function keyScopeReason(t: TFunction, verdict: KeyScopeVerdict | null): string {
  if (!verdict) return ''
  if (!verdict.reason_code) return verdict.reason
  const failed = verdict.reason_params?.levels ?? []
  return t(`keys.scope.verdict.${verdict.reason_code}`, {
    ...(verdict.reason_params ?? {}),
    // The levels arrive as their names ("user", "team"), because only the console knows what
    // they are called here and in which order the language puts them.
    levels: joinNames(failed.map((l) => t(`keys.scope.levelName.${l}`, { defaultValue: l }))),
    defaultValue: verdict.reason,
  })
}
