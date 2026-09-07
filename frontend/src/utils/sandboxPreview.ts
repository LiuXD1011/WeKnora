/**
 * A fetched response's CSP headers are NOT carried into a new Blob URL.
 * Put the policy before any untrusted markup, including malformed/duplicate
 * head tags. The iframe must separately keep sandbox="allow-scripts" (opaque
 * origin, no parent access, forms, popups or top navigation).
 *
 * Inline scripts/styles keep self-contained presentation previews interactive.
 * External scripts, fetch, images, subframes and forms are not required by the
 * generated artifact contract. Removing this meta later does not undo CSP.
 */
export const SANDBOX_PREVIEW_CSP = [
  "default-src 'none'",
  "script-src 'unsafe-inline'",
  "style-src 'unsafe-inline'",
  'img-src data: blob:',
  'font-src data:',
  'media-src data: blob:',
  "connect-src 'none'",
  "frame-src 'none'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'none'",
].join('; ')

export function buildSandboxPreviewDocument(html: string): string {
  return '<!doctype html><html><head><meta charset="utf-8">' +
    `<meta http-equiv="Content-Security-Policy" content="${SANDBOX_PREVIEW_CSP}">` +
    '<meta name="referrer" content="no-referrer"></head><body>' +
    html + '</body></html>'
}
