import assert from 'node:assert/strict'
import test from 'node:test'
import { buildSandboxPreviewDocument, SANDBOX_PREVIEW_CSP } from './sandboxPreview'

test('preview policy precedes every byte of the untrusted document', () => {
  const input = '</head><script>window.probe = true</script><html><head><base href="https://example.invalid/">'
  const result = buildSandboxPreviewDocument(input)
  assert.ok(result.indexOf('Content-Security-Policy') < result.indexOf(input))
  assert.ok(result.endsWith(input + '</body></html>'))
  for (const directive of ["connect-src 'none'", "frame-src 'none'", "base-uri 'none'", "form-action 'none'"]) {
    assert.ok(SANDBOX_PREVIEW_CSP.includes(directive))
  }
  assert.ok(!SANDBOX_PREVIEW_CSP.includes('https:'))
})
