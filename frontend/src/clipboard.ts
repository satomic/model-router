/**
 * Copy text to the clipboard, on every origin this console actually runs on.
 *
 * `navigator.clipboard` is **undefined**, not merely restricted, outside a secure context, and
 * plain http on a LAN address is exactly how a shared deployment of this console gets opened.
 * Reading `.writeText` off undefined throws synchronously inside the click handler, which is why
 * a button written as `navigator.clipboard?.writeText(t).then(...)` appears to do nothing at all:
 * the optional chain short-circuits to undefined and `.then` is the throw.
 *
 * So the API is tried when it exists, and a hidden textarea plus the legacy `execCommand('copy')`
 * carries the rest. execCommand is deprecated but is the only thing that works there, and the
 * boolean result is honest enough to report failure from.
 */
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // Fall through: a denied permission or a non-focused document both land here, and the
      // textarea path is not subject to either.
    }
  }
  return legacyCopy(text)
}

function legacyCopy(text: string): boolean {
  const area = document.createElement('textarea')
  area.value = text
  // Off-screen rather than display:none or hidden: the selection has to be real for the command
  // to have anything to copy. readOnly keeps the mobile keyboard from appearing.
  area.setAttribute('readonly', '')
  area.style.position = 'fixed'
  area.style.top = '-1000px'
  area.style.opacity = '0'
  document.body.appendChild(area)
  try {
    area.select()
    area.setSelectionRange(0, text.length)
    return document.execCommand('copy')
  } catch {
    return false
  } finally {
    document.body.removeChild(area)
  }
}
