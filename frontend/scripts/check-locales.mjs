/**
 * Key-parity gate for the translation catalogs.
 *
 * en.json is the source of truth. Every other catalog must have exactly the same set of
 * leaf keys -- a missing key silently falls back to English (which looks like a bug to
 * the user), and an orphaned key is dead weight nobody will ever notice.
 *
 * Run: node frontend/scripts/check-locales.mjs
 */
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const LOCALES_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', 'src', 'i18n', 'locales')
const SOURCE = 'en'
const TARGETS = ['zh-Hans', 'zh-Hant', 'ja', 'ko']

function load(name) {
  return JSON.parse(readFileSync(join(LOCALES_DIR, `${name}.json`), 'utf8'))
}

/** Flatten to dotted leaf paths, so nesting differences show up as key differences. */
function leafKeys(obj, prefix = '', out = new Set()) {
  for (const [k, v] of Object.entries(obj)) {
    const path = prefix ? `${prefix}.${k}` : k
    if (v && typeof v === 'object' && !Array.isArray(v)) leafKeys(v, path, out)
    else out.add(path)
  }
  return out
}

/**
 * i18next plural suffixes. English carries `_one`/`_other`; CJK has no plural
 * distinction and carries `_other` only, so a missing `_one` in a CJK catalog is
 * correct rather than an error.
 */
const CJK_OPTIONAL_SUFFIXES = ['_one']
const isCjk = (locale) => locale !== 'en'

function report(locale, source, target) {
  const missing = [...source].filter((k) => !target.has(k))
    .filter((k) => !(isCjk(locale) && CJK_OPTIONAL_SUFFIXES.some((s) => k.endsWith(s))))
  const orphan = [...target].filter((k) => !source.has(k))
  return { missing, orphan }
}

const source = leafKeys(load(SOURCE))
let failed = false

console.log(`source ${SOURCE}.json: ${source.size} keys`)
for (const locale of TARGETS) {
  const target = leafKeys(load(locale))
  const { missing, orphan } = report(locale, source, target)
  if (missing.length === 0 && orphan.length === 0) {
    console.log(`  OK   ${locale}: ${target.size} keys`)
    continue
  }
  failed = true
  console.log(`  FAIL ${locale}: ${target.size} keys`)
  for (const k of missing) console.log(`         missing: ${k}`)
  for (const k of orphan) console.log(`         orphan:  ${k}`)
}

if (failed) {
  console.error('\nlocale catalogs are out of sync')
  process.exit(1)
}
console.log('\nall catalogs in sync')
