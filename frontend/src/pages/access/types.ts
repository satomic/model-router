import type { AuthConfig } from '../../api'

/** The shared contract for every access-control sub-page.
 *
 *  Same reasoning as pages/config: saving writes the whole auth section back in one request, so the
 *  draft state has to live in AccessPage and the sub-pages stay stateless presentational
 *  components -- otherwise switching sub-pages would drop unsaved edits.
 */
export interface AccessSectionProps {
  auth: AuthConfig
  /** Merge a patch into the auth section. */
  set: (patch: Partial<AuthConfig>) => void
  notify: (kind: 'ok' | 'error', msg: string) => void
  /** The server's current (already saved) values, used to tell whether a field is a fresh draft. */
  saved: AuthConfig
}
