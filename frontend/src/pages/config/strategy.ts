import type { Strategy } from '../../api'

/** Which halves of the routing configuration a strategy actually consults.
 *
 *  Both predicates exist because `rule-then-ai` made "active" stop being the negation of the
 *  other panel's state: under it the rules and the decision model are both live. Written once
 *  here so the three panels that show an "inactive" badge cannot drift apart -- each of them
 *  used to derive it from its own `strategy === …` comparison, and a fourth strategy would have
 *  had to be remembered in each place.
 */
export const rulesActive = (strategy: Strategy): boolean =>
  strategy === 'rule' || strategy === 'rule-then-ai'

export const aiRouterActive = (strategy: Strategy): boolean =>
  strategy === 'ai' || strategy === 'rule-then-ai'
