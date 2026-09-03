<template>
  <t-drawer
    v-model:visible="internalVisible"
    placement="right"
    size="820px"
    attach="body"
    :footer="false"
    :destroy-on-close="false"
    class="sandbox-workbench-drawer"
    @close="close"
  >
    <template #header>
      <div class="workbench-header">
        <span class="workbench-title"><t-icon name="code" /> 可视化沙箱工作台</span>
        <t-tag v-if="info" size="small" variant="light">{{ info.backend }}</t-tag>
        <span v-if="info" class="workbench-root">{{ info.artifact_root }}</span>
      </div>
    </template>

    <div v-if="initialLoading" class="workbench-state"><t-loading /> 正在连接会话沙箱…</div>
    <div v-else-if="unavailable" class="workbench-state workbench-state--empty">
      <t-icon name="info-circle" size="30px" />
      <strong>当前会话还没有可用沙箱</strong>
      <span>请先使用已配置沙箱的 Agent 执行一次技能，然后重新打开工作台。</span>
      <t-button size="small" variant="outline" @click="loadInfo">重新检测</t-button>
    </div>
    <t-tabs v-else v-model="activeTab" class="workbench-tabs">
      <t-tab-panel value="terminal" label="终端">
        <SandboxTerminal
          v-if="info?.interactive"
          ref="interactiveTerminal"
          :session-id="sessionId"
          :backend="info.backend"
          :visible="visible"
        />
        <section v-else class="terminal-panel">
          <div class="terminal-mode-hint">
            当前 {{ info?.backend }} 后端不支持交互式终端，已降级为命令模式：每条命令独立执行并返回聚合输出。
          </div>
          <div ref="terminalOutputEl" class="terminal-output" role="log" aria-live="polite">
            <div v-if="!terminalLines.length" class="terminal-placeholder">
              终端命令将在会话绑定的 {{ info?.backend }} 沙箱内执行。
            </div>
            <div v-for="(line, index) in terminalLines" :key="index" :class="`terminal-line terminal-line--${line.kind}`">
              {{ line.text }}
            </div>
          </div>
          <form class="terminal-command" @submit.prevent="runCommand">
            <span class="terminal-prompt">$</span>
            <input
              v-model="command"
              autocomplete="off"
              spellcheck="false"
              :disabled="commandRunning"
              placeholder="输入命令，例如：ls -la /workspace/output"
            />
            <t-button v-if="!commandRunning" theme="primary" type="submit" :disabled="!command.trim()">运行</t-button>
            <t-button v-else theme="danger" variant="outline" type="button" @click="interruptCommand">中断</t-button>
          </form>
          <div class="terminal-options">
            <label>工作目录 <input v-model="workDir" placeholder="默认为 /workspace" /></label>
            <label>超时 <input v-model.number="timeoutSeconds" type="number" min="1" max="300" /> 秒</label>
          </div>
        </section>
      </t-tab-panel>

      <t-tab-panel value="files" label="文件">
        <section class="files-panel">
          <div class="files-toolbar">
            <span class="files-summary">产物目录 · {{ files.length }} 项</span>
            <input ref="uploadInput" type="file" class="sr-only" @change="uploadFile" />
            <t-button size="small" variant="outline" @click="uploadInput?.click()"><template #icon><t-icon name="upload" /></template>上传</t-button>
            <t-button size="small" variant="text" :loading="filesLoading" @click="loadFiles"><template #icon><t-icon name="refresh" /></template>刷新</t-button>
          </div>
          <nav v-if="currentDir" class="files-breadcrumbs" aria-label="目录路径">
            <t-button size="small" variant="text" shape="square" title="返回上级" @click="navigateTo(parentDir(currentDir))">
              <template #icon><t-icon name="arrow-up" /></template>
            </t-button>
            <t-button size="small" variant="text" @click="navigateTo('')">output</t-button>
            <template v-for="(segment, index) in currentDir.split('/')" :key="index">
              <span class="crumb-separator">/</span>
              <t-button size="small" variant="text" @click="navigateTo(currentDir.split('/').slice(0, index + 1).join('/'))">
                {{ segment }}
              </t-button>
            </template>
          </nav>
          <div v-if="filesLoading" class="workbench-state"><t-loading /> 正在读取文件…</div>
          <div v-else-if="!files.length" class="workbench-state workbench-state--empty">
            <t-icon name="folder-open" size="30px" />
            <span>产物目录为空</span>
          </div>
          <ul v-else class="workbench-file-list">
            <li v-for="file in files" :key="file.path" class="workbench-file-row">
              <button class="file-main" type="button" :disabled="file.type === 'other'" @click="openEntry(file)">
                <t-icon :name="file.type === 'dir' ? 'folder-open' : getFileIcon(file.name)" />
                <span class="file-name" :title="file.path">{{ file.type === 'dir' ? `${file.path}/` : file.path }}</span>
                <span class="file-size">{{ file.type === 'dir' ? '目录' : formatBytes(file.size) }}</span>
              </button>
              <div class="file-actions" v-if="file.type === 'file'">
                <t-button size="small" variant="text" shape="square" title="预览" @click.stop="previewFile(file)"><template #icon><t-icon name="browse" /></template></t-button>
                <t-button size="small" variant="text" shape="square" title="下载" @click.stop="downloadFile(file)"><template #icon><t-icon name="download" /></template></t-button>
                <t-button size="small" variant="text" shape="square" title="重命名" @click.stop="renameFile(file)"><template #icon><t-icon name="edit-1" /></template></t-button>
                <t-button size="small" variant="text" shape="square" theme="danger" title="删除" @click.stop="deleteFile(file)"><template #icon><t-icon name="delete" /></template></t-button>
              </div>
            </li>
          </ul>
        </section>
      </t-tab-panel>

      <t-tab-panel value="preview" label="预览">
        <section class="preview-panel">
          <div v-if="!preview" class="workbench-state workbench-state--empty">
            <t-icon name="browse" size="30px" />
            <span>请从“文件”页选择一个产物</span>
          </div>
          <template v-else>
            <div class="preview-toolbar">
              <strong :title="preview.file.path">{{ preview.file.name }}</strong>
              <t-button size="small" variant="outline" @click="downloadFile(preview.file)">下载</t-button>
            </div>
            <div v-if="preview.loading" class="workbench-state"><t-loading /> 正在生成预览…</div>
            <iframe
              v-else-if="preview.kind === 'html'"
              class="preview-frame"
              :src="preview.url"
              sandbox="allow-scripts"
              referrerpolicy="no-referrer"
              title="沙箱网页产物预览"
            />
            <iframe v-else-if="preview.kind === 'pdf'" class="preview-frame" :src="preview.url" title="PDF 预览" />
            <img v-else-if="preview.kind === 'image'" class="preview-image" :src="preview.url" alt="产物预览" />
            <VueOfficePptx v-else-if="preview.kind === 'pptx'" :src="preview.buffer" class="office-preview" />
            <div v-else-if="preview.kind === 'sheet'" class="sheet-wrap">
              <table class="sheet-table">
                <tbody>
                  <tr v-for="(row, rowIndex) in preview.rows" :key="rowIndex">
                    <td v-for="(cell, colIndex) in row" :key="colIndex">{{ cell }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
            <pre v-else-if="preview.kind === 'text'" class="text-preview">{{ preview.text }}</pre>
            <div v-else class="workbench-state workbench-state--empty">该文件暂不支持内嵌预览，请下载后查看。</div>
          </template>
        </section>
      </t-tab-panel>
    </t-tabs>
  </t-drawer>
</template>

<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, watch } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import VueOfficePptx from '@vue-office/pptx'
import * as XLSX from 'xlsx'
import { getFileIcon } from '@/utils/files'
import SandboxTerminal from './SandboxTerminal.vue'
import {
  deleteSandboxWorkbenchFile,
  downloadSandboxWorkbenchFile,
  executeSandboxWorkbenchCommand,
  getSandboxWorkbenchInfo,
  listSandboxWorkbenchFiles,
  renameSandboxWorkbenchFile,
  uploadSandboxWorkbenchFile,
  type SandboxWorkbenchFile,
  type SandboxWorkbenchInfo,
} from '@/api/chat/sandbox-workbench'

type TerminalLine = { kind: 'command' | 'stdout' | 'stderr' | 'status'; text: string }
type PreviewKind = 'html' | 'pdf' | 'image' | 'pptx' | 'sheet' | 'text' | 'unsupported'
type PreviewState = {
  file: SandboxWorkbenchFile
  kind: PreviewKind
  loading: boolean
  url: string
  buffer?: ArrayBuffer
  rows: unknown[][]
  text: string
}

const props = defineProps<{ visible: boolean; sessionId: string }>()
const emit = defineEmits<{ (event: 'update:visible', value: boolean): void }>()

const internalVisible = computed({
  get: () => props.visible,
  set: value => emit('update:visible', value),
})
const activeTab = ref('terminal')
const initialLoading = ref(false)
const unavailable = ref(false)
const info = ref<SandboxWorkbenchInfo | null>(null)
const files = ref<SandboxWorkbenchFile[]>([])
const filesLoading = ref(false)
// currentDir is the browsed subdirectory relative to /workspace/output.
// Empty string is the artifact root itself.
const currentDir = ref('')
const uploadInput = ref<HTMLInputElement | null>(null)
const interactiveTerminal = ref<InstanceType<typeof SandboxTerminal> | null>(null)
const terminalOutputEl = ref<HTMLElement | null>(null)
const terminalLines = ref<TerminalLine[]>([])
const command = ref('')
const workDir = ref('')
const timeoutSeconds = ref(60)
const commandRunning = ref(false)
const preview = ref<PreviewState | null>(null)
let commandAbort: AbortController | null = null

watch(() => props.visible, open => {
  if (open) void loadInfo()
})
watch(activeTab, tab => {
  if (tab === 'files') void loadFiles()
})

async function loadInfo() {
  if (!props.sessionId) return
  initialLoading.value = true
  unavailable.value = false
  currentDir.value = ''
  try {
    const response = await getSandboxWorkbenchInfo(props.sessionId)
    info.value = response.data
    if (!response.data.terminal && !response.data.files) unavailable.value = true
  } catch {
    info.value = null
    unavailable.value = true
  } finally {
    initialLoading.value = false
  }
}

async function loadFiles() {
  if (!props.sessionId || !info.value?.files) return
  filesLoading.value = true
  try {
    const response = await listSandboxWorkbenchFiles(props.sessionId, currentDir.value)
    files.value = Array.isArray(response.data) ? response.data : []
  } catch (error: any) {
    MessagePlugin.error(error?.message || '读取沙箱文件失败')
  } finally {
    filesLoading.value = false
  }
}

function openEntry(file: SandboxWorkbenchFile) {
  if (file.type === 'dir') {
    void navigateTo(file.path)
  } else {
    void previewFile(file)
  }
}

async function navigateTo(dir: string) {
  if (currentDir.value === dir) {
    void loadFiles()
    return
  }
  currentDir.value = dir
  await loadFiles()
}

function parentDir(dir: string) {
  const index = dir.lastIndexOf('/')
  return index > 0 ? dir.slice(0, index) : ''
}

function appendTerminal(kind: TerminalLine['kind'], text: string) {
  if (!text) return
  terminalLines.value.push({ kind, text })
  nextTick(() => {
    const el = terminalOutputEl.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

async function runCommand() {
  const value = command.value.trim()
  if (!value || commandRunning.value || !props.sessionId) return
  command.value = ''
  appendTerminal('command', `$ ${value}`)
  commandAbort = new AbortController()
  commandRunning.value = true
  try {
    const response = await executeSandboxWorkbenchCommand(
      props.sessionId, value, workDir.value.trim(), timeoutSeconds.value, commandAbort.signal,
    )
    const result = response.data
    appendTerminal('stdout', result.stdout)
    appendTerminal('stderr', result.stderr || result.error || '')
    appendTerminal('status', `[退出码 ${result.exit_code} · ${result.duration_ms} ms${result.killed ? ' · 已终止' : ''}]`)
    if (/\b(output|workspace)\b/.test(value)) void loadFiles()
  } catch (error: any) {
    appendTerminal('stderr', commandAbort?.signal.aborted ? '[命令已中断]' : `[执行失败] ${error?.message || 'unknown error'}`)
  } finally {
    commandRunning.value = false
    commandAbort = null
  }
}

function interruptCommand() {
  commandAbort?.abort()
}

async function uploadFile(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file || !props.sessionId) return
  try {
    await uploadSandboxWorkbenchFile(props.sessionId, file.name, file)
    MessagePlugin.success('文件已上传')
    await loadFiles()
  } catch (error: any) {
    MessagePlugin.error(error?.message || '上传失败')
  } finally {
    input.value = ''
  }
}

async function downloadFile(file: SandboxWorkbenchFile) {
  try {
    const blob = await downloadSandboxWorkbenchFile(props.sessionId, file.path)
    const url = URL.createObjectURL(blob)
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = file.name
    document.body.appendChild(anchor)
    anchor.click()
    anchor.remove()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  } catch (error: any) {
    MessagePlugin.error(error?.message || '下载失败')
  }
}

async function renameFile(file: SandboxWorkbenchFile) {
  const nextPath = window.prompt('新的相对路径', file.path)?.trim()
  if (!nextPath || nextPath === file.path) return
  try {
    await renameSandboxWorkbenchFile(props.sessionId, file.path, nextPath)
    MessagePlugin.success('重命名成功')
    await loadFiles()
  } catch (error: any) {
    MessagePlugin.error(error?.message || '重命名失败')
  }
}

async function deleteFile(file: SandboxWorkbenchFile) {
  if (!window.confirm(`删除 ${file.path}？`)) return
  try {
    await deleteSandboxWorkbenchFile(props.sessionId, file.path)
    if (preview.value?.file.path === file.path) clearPreview()
    MessagePlugin.success('文件已删除')
    await loadFiles()
  } catch (error: any) {
    MessagePlugin.error(error?.message || '删除失败')
  }
}

function previewKind(name: string, mimeType: string): PreviewKind {
  const ext = name.split('.').pop()?.toLowerCase() || ''
  if (['html', 'htm'].includes(ext)) return 'html'
  if (ext === 'pdf') return 'pdf'
  if (['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg'].includes(ext)) return 'image'
  if (['pptx', 'ppt'].includes(ext)) return 'pptx'
  if (['xlsx', 'xls', 'csv', 'tsv'].includes(ext)) return 'sheet'
  if (mimeType.startsWith('text/') || ['md', 'json', 'yaml', 'yml', 'log', 'txt'].includes(ext)) return 'text'
  return 'unsupported'
}

async function previewFile(file: SandboxWorkbenchFile) {
  clearPreview()
  preview.value = { file, kind: 'unsupported', loading: true, url: '', rows: [], text: '' }
  activeTab.value = 'preview'
  try {
    const blob = await downloadSandboxWorkbenchFile(props.sessionId, file.path)
    if (!preview.value || preview.value.file.path !== file.path) return
    const kind = previewKind(file.name, blob.type)
    preview.value.kind = kind
    if (['html', 'pdf', 'image'].includes(kind)) {
      preview.value.url = URL.createObjectURL(blob)
    } else if (kind === 'pptx') {
      preview.value.buffer = await blob.arrayBuffer()
    } else if (kind === 'sheet') {
      const workbook = XLSX.read(await blob.arrayBuffer(), { type: 'array' })
      const first = workbook.Sheets[workbook.SheetNames[0]]
      preview.value.rows = first ? XLSX.utils.sheet_to_json(first, { header: 1, blankrows: false }) : []
    } else if (kind === 'text') {
      preview.value.text = await blob.text()
    }
  } catch (error: any) {
    MessagePlugin.error(error?.message || '预览失败')
    clearPreview()
  } finally {
    if (preview.value?.file.path === file.path) preview.value.loading = false
  }
}

function clearPreview() {
  if (preview.value?.url) URL.revokeObjectURL(preview.value.url)
  preview.value = null
}

function close() {
  interruptCommand()
  emit('update:visible', false)
}

function formatBytes(value: number) {
  if (!value) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  let size = value
  let index = 0
  while (size >= 1024 && index < units.length - 1) {
    size /= 1024
    index++
  }
  return `${index ? size.toFixed(1) : size} ${units[index]}`
}

onUnmounted(() => {
  interruptCommand()
  clearPreview()
})
</script>

<style scoped lang="less">
.workbench-header { display: flex; align-items: center; gap: 10px; min-width: 0; }
.workbench-title { display: inline-flex; align-items: center; gap: 7px; font-weight: 650; }
.workbench-root { color: var(--td-text-color-placeholder); font: 12px ui-monospace, monospace; }
.workbench-tabs { height: 100%; display: flex; flex-direction: column; }
.workbench-tabs :deep(.t-tabs__content) { flex: 1; min-height: 0; }
.workbench-tabs :deep(.t-tab-panel) { height: 100%; }
.workbench-state { min-height: 180px; display: flex; align-items: center; justify-content: center; gap: 10px; color: var(--td-text-color-secondary); }
.workbench-state--empty { flex-direction: column; text-align: center; }
.terminal-panel, .files-panel, .preview-panel { height: 100%; min-height: 0; display: flex; flex-direction: column; padding-top: 12px; }
.terminal-mode-hint { margin-bottom: 10px; padding: 8px 12px; border-radius: 6px; background: var(--td-bg-color-secondarycontainer); color: var(--td-text-color-secondary); font-size: 12px; }
.files-breadcrumbs { display: flex; align-items: center; gap: 2px; min-height: 32px; border-bottom: 1px solid var(--td-component-stroke); color: var(--td-text-color-secondary); }
.crumb-separator { color: var(--td-text-color-placeholder); }
.terminal-output { flex: 1; min-height: 360px; overflow: auto; padding: 16px; border-radius: 8px; background: #101418; color: #d7e0e7; font: 13px/1.55 ui-monospace, SFMono-Regular, Consolas, monospace; white-space: pre-wrap; overflow-wrap: anywhere; }
.terminal-placeholder { color: #80909b; }
.terminal-line--command { color: #66d9a7; margin-top: 8px; }
.terminal-line--stderr { color: #ff8d8d; }
.terminal-line--status { color: #8fa0ab; }
.terminal-command { display: grid; grid-template-columns: auto 1fr auto; gap: 8px; align-items: center; margin-top: 10px; }
.terminal-prompt { color: var(--td-brand-color); font: 700 18px ui-monospace, monospace; }
.terminal-command input, .terminal-options input { border: 1px solid var(--td-component-stroke); border-radius: 6px; background: var(--td-bg-color-container); color: var(--td-text-color-primary); outline: none; }
.terminal-command input { height: 34px; padding: 0 10px; font: 13px ui-monospace, monospace; }
.terminal-command input:focus, .terminal-options input:focus { border-color: var(--td-brand-color); }
.terminal-options { display: flex; gap: 18px; margin-top: 10px; color: var(--td-text-color-secondary); font-size: 12px; }
.terminal-options label { display: inline-flex; align-items: center; gap: 6px; }
.terminal-options input { width: 150px; height: 28px; padding: 0 8px; }
.terminal-options input[type='number'] { width: 64px; }
.files-toolbar, .preview-toolbar { display: flex; align-items: center; gap: 8px; min-height: 36px; border-bottom: 1px solid var(--td-component-stroke); padding-bottom: 10px; }
.files-summary, .preview-toolbar strong { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.workbench-file-list { list-style: none; padding: 0; margin: 8px 0 0; overflow: auto; }
.workbench-file-row { display: flex; align-items: center; gap: 8px; min-height: 44px; border-bottom: 1px solid var(--td-component-stroke); }
.file-main { flex: 1; min-width: 0; display: grid; grid-template-columns: 22px 1fr auto; align-items: center; gap: 8px; border: 0; background: transparent; color: var(--td-text-color-primary); text-align: left; cursor: pointer; }
.file-main:disabled { cursor: default; }
.file-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font: 13px ui-monospace, monospace; }
.file-size { color: var(--td-text-color-placeholder); font-size: 12px; }
.file-actions { display: flex; }
.preview-toolbar { flex-shrink: 0; }
.preview-frame, .office-preview { flex: 1; width: 100%; min-height: 520px; border: 0; margin-top: 10px; background: white; }
.preview-image { max-width: 100%; max-height: calc(100vh - 170px); object-fit: contain; margin: 10px auto 0; }
.sheet-wrap { flex: 1; overflow: auto; margin-top: 10px; border: 1px solid var(--td-component-stroke); }
.sheet-table { border-collapse: collapse; min-width: 100%; font-size: 12px; }
.sheet-table td { padding: 6px 9px; border: 1px solid var(--td-component-stroke); white-space: nowrap; }
.sheet-table tr:first-child td { position: sticky; top: 0; background: var(--td-bg-color-secondarycontainer); font-weight: 650; }
.text-preview { flex: 1; overflow: auto; margin: 10px 0 0; padding: 14px; border-radius: 8px; background: var(--td-bg-color-secondarycontainer); white-space: pre-wrap; overflow-wrap: anywhere; font: 13px/1.55 ui-monospace, monospace; }
.sr-only { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip: rect(0, 0, 0, 0); white-space: nowrap; border: 0; }
</style>

<style lang="less">
.sandbox-workbench-drawer.t-drawer .t-drawer__body { height: calc(100vh - 68px); padding: 0 20px 18px; overflow: hidden; }
</style>
