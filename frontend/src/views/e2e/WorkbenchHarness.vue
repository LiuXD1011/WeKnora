<template>
  <!--
    DEV-ONLY E2E harness: mounts the sandbox workbench drawer standalone so
    Playwright can drive the real component without a live backend, a chat
    session, or credentials. The route is registered only when
    import.meta.env.DEV is true (see src/router/index.ts); production builds
    tree-shake it away. All API/WS traffic is intercepted by the tests.
  -->
  <div class="e2e-harness">
    <SandboxWorkbenchDrawer v-model:visible="visible" :session-id="sessionId" />
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useRoute } from 'vue-router'
import SandboxWorkbenchDrawer from '../chat/components/SandboxWorkbenchDrawer.vue'

const route = useRoute()
const visible = ref(false)
// The session id doubles as the scenario switch the tests intercept on:
// e2e-interactive (docker + PTY), e2e-exec (degraded command mode),
// e2e-none (no live sandbox).
const sessionId = ref(String(route.query.session || 'e2e-interactive'))

// Flip visibility on the next macrotask so the drawer's visible watcher
// fires and triggers its initial capability load.
setTimeout(() => {
  visible.value = true
}, 50)
</script>

<style scoped>
.e2e-harness {
  min-height: 100vh;
  background: var(--td-bg-color-page, #f5f7fa);
}
</style>
