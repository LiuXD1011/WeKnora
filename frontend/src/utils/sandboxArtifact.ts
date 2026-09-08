import type { SandboxWorkbenchFile } from '@/api/chat/sandbox-workbench'

export type SandboxPreviewKind = 'html' | 'pdf' | 'image' | 'pptx' | 'sheet' | 'text' | 'unsupported'

export function sandboxArtifactLabel(file: SandboxWorkbenchFile): string {
  const labels = { presentation: '演示文稿', webpage: '网页', spreadsheet: '表格', file: '文件' }
  return file.artifact_type && Object.hasOwn(labels, file.artifact_type) ? labels[file.artifact_type] : '文件'
}

export function sandboxArtifactPreviewKind(file: SandboxWorkbenchFile, mimeType: string): SandboxPreviewKind {
  if (file.type !== 'file') return 'unsupported'
  if (file.preview_format !== undefined) {
    // A server hint selects a renderer, never iframe permissions or code.
    const formats: Record<string, SandboxPreviewKind> = {
      html: 'html', pdf: 'pdf', image: 'image', pptx: 'pptx', spreadsheet: 'sheet', text: 'text', unsupported: 'unsupported',
    }
    return Object.hasOwn(formats, file.preview_format) ? formats[file.preview_format] : 'unsupported'
  }
  // Backwards compatibility for older API versions, visibly without claiming
  // producer-declared metadata. Legacy binary PPT is not a PPTX document.
  const ext = file.name.split('.').pop()?.toLowerCase() || ''
  if (['html', 'htm'].includes(ext)) return 'html'
  if (ext === 'pdf') return 'pdf'
  if (['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg'].includes(ext)) return 'image'
  if (ext === 'pptx') return 'pptx'
  if (['xlsx', 'xls', 'csv', 'tsv'].includes(ext)) return 'sheet'
  if (mimeType.startsWith('text/') || ['md', 'json', 'yaml', 'yml', 'log', 'txt'].includes(ext)) return 'text'
  return 'unsupported'
}
