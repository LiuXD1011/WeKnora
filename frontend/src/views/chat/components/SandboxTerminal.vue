<template>
  <section class="interactive-terminal">
    <div class="terminal-status">
      <span v-if="status === 'connecting'" class="status-badge status-connecting">
        <t-loading size="14px" /> 正在连接 {{ backend }} 沙箱终端…
      </span>
      <span v-else-if="status === 'live'" class="status-badge status-live">
        <span class="status-dot" /> 已连接 · {{ backend }} · {{ terminalId }}
      </span>
      <span v-else-if="status === 'ended'" class="status-badge status-ended">
        <span class="status-dot" /> 会话已结束（{{ exitReasonLabel }}）· 退出码 {{ exitCode }}
      </span>
      <t-button size="small" variant="outline" :disabled="status === 'connecting'" @click="restart">
        {{ status === 'ended' ? '重新连接' : '重连' }}
      </t-button>
      <t-button size="small" variant="text" @click="clearScreen">清屏</t-button>
      <span class="terminal-hint">Ctrl-C 中断 · 拖动窗口自动适配</span>
    </div>
    <div ref="hostEl" class="terminal-host" data-interactive-terminal></div>
  </section>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { MessagePlugin } from 'tdesign-vue-next'
import { openSandboxWorkbenchTerminal } from '@/api/chat/sandbox-workbench'

type TerminalStatus = 'connecting' | 'live' | 'ended'

const EXIT_REASON_LABELS: Record<string, string> = {
  process_exit: '进程退出',
  client_disconnect: '连接断开',
  lease_expired: '达到时长上限',
  connection_lost: '连接丢失',
  backend_error: '后端异常',
  write_failed: '写入失败',
  connection_closed: '连接关闭',
}

const props = defineProps<{ sessionId: string; backend: string; visible: boolean }>()

const hostEl = ref<HTMLElement | null>(null)
const status = ref<TerminalStatus>('connecting')
const terminalId = ref('')
const exitCode = ref(0)
const exitReason = ref('')

let term: Terminal | null = null
let fitAddon: FitAddon | null = null
let socket: WebSocket | null = null
let resizeObserver: ResizeObserver | null = null
let resizeTimer: number | null = null
let keepAliveTimer: number | null = null
let reconnectTimer: number | null = null

const exitReasonLabel = ref('连接关闭')

function emitOutput(data: Uint8Array) {
  term?.write(data)
}

function sendSocketData(data: string | Uint8Array) {
  if (!socket || socket.readyState !== WebSocket.OPEN) return
  if (typeof data === 'string') {
    socket.send(data)
    return
  }
  // Copy into a plain ArrayBuffer: recent DOM typings reject
  // Uint8Array<ArrayBufferLike> directly on WebSocket.send.
  const buffer = new ArrayBuffer(data.byteLength)
  new Uint8Array(buffer).set(data)
  socket.send(buffer)
}

function scheduleFit() {
  if (resizeTimer !== null) return
  resizeTimer = window.setTimeout(() => {
    resizeTimer = null
    if (!fitAddon || !term) return
    try {
      fitAddon.fit()
    } catch {
      // The host can be detached mid-layout during drawer transitions.
      return
    }
    const { cols, rows } = term
    if (socket?.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify({ type: 'resize', cols, rows }))
    }
  }, 50)
}

function startKeepAlive() {
  stopKeepAlive()
  // The server drops the terminal when no frame arrives for 75s; ping well
  // inside that budget so an idle shell is not reaped.
  keepAliveTimer = window.setInterval(() => {
    sendSocketData(JSON.stringify({ type: 'ping', seq: Date.now() }))
  }, 25000)
}

function stopKeepAlive() {
  if (keepAliveTimer !== null) {
    window.clearInterval(keepAliveTimer)
    keepAliveTimer = null
  }
}

function teardown() {
  stopKeepAlive()
  if (resizeTimer !== null) {
    window.clearTimeout(resizeTimer)
    resizeTimer = null
  }
  if (socket) {
    socket.onopen = socket.onmessage = socket.onerror = socket.onclose = null
    if (socket.readyState <= WebSocket.OPEN) socket.close()
    socket = null
  }
}

function connect() {
  if (!hostEl.value) return
  teardown()
  status.value = 'connecting'

  if (!term) {
    term = new Terminal({
      fontFamily: 'ui-monospace, SFMono-Regular, Consolas, monospace',
      fontSize: 13,
      cursorBlink: true,
      scrollback: 5000,
      theme: { background: '#101418', foreground: '#d7e0e7' },
    })
    fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.onData(data => sendSocketData(new TextEncoder().encode(data)))
    term.open(hostEl.value)
    fitAddon.fit()
  } else {
    term.reset()
  }
  term.focus()

  const { cols, rows } = term
  const opened = openSandboxWorkbenchTerminal(props.sessionId, cols, rows)
  socket = opened.socket
  socket.binaryType = 'arraybuffer'

  socket.onopen = () => {
    status.value = 'live'
    scheduleFit()
    startKeepAlive()
  }
  socket.onmessage = event => {
    if (typeof event.data === 'string') {
      try {
        const message = JSON.parse(event.data)
        switch (message.type) {
          case 'ready':
            terminalId.value = message.terminal_id || ''
            status.value = 'live'
            break
          case 'exit':
            status.value = 'ended'
            exitCode.value = message.code ?? 0
            exitReason.value = message.reason || 'connection_closed'
            exitReasonLabel.value = EXIT_REASON_LABELS[exitReason.value] || exitReason.value
            term?.write('\r\n\x1b[33m[终端会话已结束]\x1b[0m\r\n')
            break
          case 'error':
            status.value = 'ended'
            term?.write(`\r\n\x1b[31m[错误] ${message.message || message.error}\x1b[0m\r\n`)
            break
          case 'pong':
            break
        }
      } catch {
        // Ignore malformed control frames; they never carry terminal output.
      }
      return
    }
    emitOutput(new Uint8Array(event.data as ArrayBuffer))
  }
  socket.onerror = () => {
    if (status.value !== 'ended') {
      MessagePlugin.error('终端连接异常，请尝试重连')
    }
  }
  socket.onclose = () => {
    stopKeepAlive()
    if (status.value === 'live') {
      status.value = 'ended'
      exitReason.value = 'connection_lost'
      exitReasonLabel.value = EXIT_REASON_LABELS.connection_lost
      term?.write('\r\n\x1b[33m[连接已断开]\x1b[0m\r\n')
    }
  }
}

function restart() {
  connect()
}

function clearScreen() {
  term?.clear()
  term?.focus()
}

onMounted(() => {
  connect()
  resizeObserver = new ResizeObserver(() => scheduleFit())
  if (hostEl.value) resizeObserver.observe(hostEl.value)
})

onUnmounted(() => {
  if (resizeObserver) {
    resizeObserver.disconnect()
    resizeObserver = null
  }
  teardown()
  term?.dispose()
  term = null
  fitAddon = null
})

defineExpose({ restart })
</script>

<style scoped lang="less">
.interactive-terminal {
  height: 100%;
  min-height: 0;
  display: flex;
  flex-direction: column;
  padding-top: 12px;
}
.terminal-status {
  display: flex;
  align-items: center;
  gap: 10px;
  min-height: 32px;
  padding-bottom: 8px;
  border-bottom: 1px solid var(--td-component-stroke);
  color: var(--td-text-color-secondary);
  font-size: 12px;
}
.status-badge {
  flex: 1;
  min-width: 0;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.status-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--td-success-color);
}
.status-ended .status-dot {
  background: var(--td-warning-color);
}
.terminal-hint {
  color: var(--td-text-color-placeholder);
}
.terminal-host {
  flex: 1;
  min-height: 360px;
  margin-top: 10px;
  padding: 10px;
  border-radius: 8px;
  background: #101418;
}
.terminal-host :deep(.xterm) {
  height: 100%;
}
</style>
