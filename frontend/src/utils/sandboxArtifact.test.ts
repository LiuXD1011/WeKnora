import assert from 'node:assert/strict'
import test from 'node:test'
import type { SandboxWorkbenchFile } from '../api/chat/sandbox-workbench'
import { sandboxArtifactLabel, sandboxArtifactPreviewKind } from './sandboxArtifact'

const file = (extra: Partial<SandboxWorkbenchFile> = {}): SandboxWorkbenchFile => ({
  name: 'download.bin', path: 'download.bin', type: 'file', size: 1, mod_time: '', ...extra,
})

test('server artifact classification labels are distinct', () => {
  assert.equal(sandboxArtifactLabel(file({ artifact_type: 'presentation' })), '演示文稿')
  assert.equal(sandboxArtifactLabel(file({ artifact_type: 'webpage' })), '网页')
  assert.equal(sandboxArtifactLabel(file({ artifact_type: 'spreadsheet' })), '表格')
})

test('preview format is independent of filename and download MIME', () => {
  assert.equal(sandboxArtifactPreviewKind(file({ preview_format: 'html' }), 'application/octet-stream'), 'html')
  assert.equal(sandboxArtifactPreviewKind(file({ preview_format: 'spreadsheet' }), ''), 'sheet')
  assert.equal(sandboxArtifactPreviewKind(file({ name: 'fake.html', preview_format: 'unsupported' }), 'text/html'), 'unsupported')
})

test('legacy response compatibility excludes binary PPT and directories', () => {
  assert.equal(sandboxArtifactPreviewKind(file({ name: 'deck.PPTX' }), ''), 'pptx')
  assert.equal(sandboxArtifactPreviewKind(file({ name: 'deck.ppt' }), ''), 'unsupported')
  assert.equal(sandboxArtifactPreviewKind(file({ name: 'data.csv' }), ''), 'sheet')
  assert.equal(sandboxArtifactPreviewKind(file({ type: 'dir', preview_format: 'html' }), ''), 'unsupported')
})

test('unknown runtime metadata cannot select an inherited property', () => {
  const invalid = file({ preview_format: 'constructor' as any, artifact_type: '__proto__' as any })
  assert.equal(sandboxArtifactPreviewKind(invalid, ''), 'unsupported')
  assert.equal(sandboxArtifactLabel(invalid), '文件')
})
