import type { RouterConfig } from '../../api'

/** The props the model-policy panels take.
 *
 *  The same shape as the routing-configuration sections minus `goto`: this page has no sub-pages to
 *  navigate between. The whole `cfg` is passed rather than just the two policy keys because the
 *  group editor checkboxes are drawn from the **model catalog**, which the page reads but never
 *  writes -- see PolicyPage for why only the two policy keys are saved back.
 */
export interface PolicyProps {
  cfg: RouterConfig
  set: (patch: Partial<RouterConfig>) => void
  notify: (kind: 'ok' | 'error', msg: string) => void
}
