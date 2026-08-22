import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

/**
 * The row above a configuration list: a search box, whatever filters the page adds, how many
 * records the current filter leaves, and a way back to the unfiltered list.
 *
 * One component for all three list pages, because the three pages are the same problem -- the
 * catalog is short until it is not, and a search that sits somewhere different on each page is
 * a search nobody finds. `.filter-bar` is the same bar the Traces page uses.
 */
export default function ListToolbar({
  search,
  onSearch,
  placeholder,
  shown,
  total,
  filtered,
  onClear,
  children,
}: {
  search: string
  onSearch: (v: string) => void
  placeholder: string
  shown: number
  total: number
  /** True when a search term or any filter is set, which is what enables Clear. */
  filtered: boolean
  onClear: () => void
  /** The page's own filter fields, each a `<label className="filter-field">`. */
  children?: ReactNode
}) {
  const { t } = useTranslation()
  return (
    <div className="filter-bar">
      <label className="filter-field" style={{ flex: '1 1 200px', maxWidth: 320 }}>
        <span className="field-name">{t('config.list.search')}</span>
        {/* type=text, not type=search: the WebKit clear button that a search input draws is
            unstyleable, invisible against the dark theme, and absent in other browsers -- and
            the Clear button on the right of this bar resets the selects too, which it never
            would. */}
        <input
          type="text"
          value={search}
          placeholder={placeholder}
          onChange={(e) => onSearch(e.target.value)}
        />
      </label>
      {children}
      <span className="spacer" style={{ marginLeft: 'auto' }} />
      <span className="dim" style={{ fontSize: 12.5, paddingBottom: 5 }}>
        {t('config.list.showing', { shown, total })}
      </span>
      {filtered && (
        <button className="btn ghost sm" onClick={onClear}>
          {t('config.list.clear')}
        </button>
      )}
    </div>
  )
}
