import type { RouterConfig } from '../../api'

/** The props shared by every routing-configuration sub-page: the whole cfg, a partial patch, and the
 *  notify / navigate callbacks.
 *
 *  Sub-pages hold no state of their own: saving PUTs the entire config, so the draft has to live in
 *  ConfigPage -- that way switching sub-pages never loses unsaved edits. */
export interface SectionProps {
  cfg: RouterConfig
  set: (patch: Partial<RouterConfig>) => void
  notify: (kind: 'ok' | 'error', msg: string) => void
  goto: (section: string) => void
}
