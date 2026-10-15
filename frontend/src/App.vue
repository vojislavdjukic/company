<script setup>
import { computed, onMounted, ref } from 'vue'

const token = ref(localStorage.getItem('thecompany-token') || '')
const user = ref(null)
const agents = ref([])
const selectedAgent = ref('')
const messages = ref([])
const prompt = ref('')
const email = ref('')
const password = ref('')
const isRegistering = ref(false)
const busy = ref(false)
const error = ref('')
const authReady = computed(() => Boolean(token.value && user.value))

/**
 * Sends a JSON request to the Go API and returns its decoded response.
 * path is the API route; options contains fetch settings; authenticated adds the saved bearer token.
 */
async function api(path, options = {}, authenticated = true) {
  const headers = new Headers(options.headers || {})
  if (options.body) headers.set('Content-Type', 'application/json')
  if (authenticated && token.value) headers.set('Authorization', `Bearer ${token.value}`)
  const response = await fetch(path, { ...options, headers })
  const body = response.status === 204 ? null : await response.json().catch(() => null)
  if (!response.ok) throw new Error(body?.error || `Request failed (${response.status})`)
  return body
}

/**
 * Loads the signed-in account and its visible containers.
 * No parameters are required; failures clear the invalid saved session.
 */
async function loadWorkspace() {
  try {
    user.value = await api('/auth/me')
    const result = await api('/codex/agents')
    agents.value = result.containers || []
    if (!selectedAgent.value && agents.value.length) selectedAgent.value = agents.value[0].agent_name
  } catch (requestError) {
    if (requestError.message.includes('token') || requestError.message.includes('bearer')) signOut()
    error.value = requestError.message
  }
}

/**
 * Creates an account or starts a session, then loads its agent workspace.
 * No parameters are required; email, password, and mode come from the form.
 */
async function submitAuth() {
  busy.value = true
  error.value = ''
  try {
    const route = isRegistering.value ? '/auth/register' : '/auth/login'
    const result = await api(route, {
      method: 'POST',
      body: JSON.stringify({ email: email.value, password: password.value }),
    }, false)
    token.value = result.token
    localStorage.setItem('thecompany-token', result.token)
    user.value = result.user
    await loadWorkspace()
  } catch (requestError) {
    error.value = requestError.message
  } finally {
    busy.value = false
  }
}

/**
 * Clears the local session and returns the interface to the sign-in screen.
 * No parameters are required.
 */
function signOut() {
  token.value = ''
  user.value = null
  agents.value = []
  selectedAgent.value = ''
  messages.value = []
  localStorage.removeItem('thecompany-token')
}

/**
 * Creates a container-backed Codex agent and selects its generated name.
 * No parameters are required; the server allocates the agent name and thread.
 */
async function createAgent() {
  busy.value = true
  error.value = ''
  try {
    const result = await api('/codex/agents', { method: 'POST' })
    await loadWorkspace()
    selectedAgent.value = result.agent_name
    messages.value = []
  } catch (requestError) {
    error.value = requestError.message
  } finally {
    busy.value = false
  }
}

/**
 * Sends the composed prompt to the selected agent and appends Codex's reply.
 * No parameters are required; the endpoint waits until the turn is complete.
 */
async function sendMessage() {
  const text = prompt.value.trim()
  if (!text || busy.value) return
  const agentName = selectedAgent.value
  messages.value.push({ role: 'user', text })
  prompt.value = ''
  busy.value = true
  error.value = ''
  try {
    const result = await api('/codex/messages', {
      method: 'POST',
      body: JSON.stringify({ message: text, agent_name: agentName || undefined }),
    })
    selectedAgent.value = result.agent_name
    messages.value.push({ role: 'assistant', text: result.response })
    await loadWorkspace()
  } catch (requestError) {
    error.value = requestError.message
  } finally {
    busy.value = false
  }
}

/**
 * Removes the selected agent and its container, then selects another available agent.
 * No parameters are required; the current selection determines the target.
 */
async function deleteAgent() {
  if (!selectedAgent.value) return
  busy.value = true
  error.value = ''
  try {
    await api(`/codex/agents/${selectedAgent.value}`, { method: 'DELETE' })
    messages.value = []
    selectedAgent.value = ''
    await loadWorkspace()
  } catch (requestError) {
    error.value = requestError.message
  } finally {
    busy.value = false
  }
}

onMounted(() => {
  if (token.value) loadWorkspace()
})
</script>

<template>
  <main v-if="!authReady" class="auth-page">
    <section class="auth-card">
      <a class="brand" href="/"><span class="brand-mark">C</span> the company</a>
      <div class="eyebrow">YOUR AI WORKSPACE</div>
      <h1>{{ isRegistering ? 'Create your account' : 'Welcome back' }}</h1>
      <p class="muted">Sign in to pick up where your agents left off.</p>
      <form class="auth-form" @submit.prevent="submitAuth">
        <label>Email address<input v-model="email" type="email" autocomplete="email" required /></label>
        <label>Password<input v-model="password" type="password" :autocomplete="isRegistering ? 'new-password' : 'current-password'" minlength="8" required /></label>
        <p v-if="error" class="error">{{ error }}</p>
        <button class="button primary full" :disabled="busy">{{ busy ? 'Please wait…' : isRegistering ? 'Create account' : 'Sign in' }}</button>
      </form>
      <p class="switch-auth">
        {{ isRegistering ? 'Already have an account?' : 'New to the company?' }}
        <button class="text-button" @click="isRegistering = !isRegistering; error = ''">{{ isRegistering ? 'Sign in' : 'Create an account' }}</button>
      </p>
    </section>
    <div class="auth-aside"><div class="orbit orbit-one"></div><div class="orbit orbit-two"></div><div class="aside-copy"><span class="eyebrow">A TEAM THAT THINKS WITH YOU</span><h2>Good work<br />starts with a<br /><em>conversation.</em></h2><p>Give your agents a brief. Build something remarkable together.</p></div><span class="aside-note">THE COMPANY · AI WORKSPACE</span></div>
  </main>

  <main v-else class="workspace">
    <aside class="sidebar">
      <a class="brand" href="/"><span class="brand-mark">C</span> the company</a>
      <div class="side-label">WORKSPACE</div>
      <button class="new-agent button" :disabled="busy" @click="createAgent"><span>＋</span> New agent</button>
      <div class="side-label agents-label">YOUR AGENTS <span>{{ agents.length }}</span></div>
      <div class="agent-list">
        <button v-for="agent in agents" :key="agent.agent_name" class="agent-item" :class="{ active: selectedAgent === agent.agent_name }" @click="selectedAgent = agent.agent_name; messages = []; error = ''">
          <span class="agent-avatar">{{ agent.agent_name?.slice(0, 1).toUpperCase() }}</span>
          <span class="agent-copy"><strong>Agent {{ agent.agent_name }}</strong><small>{{ agent.status }}</small></span>
          <span class="status-dot"></span>
        </button>
        <p v-if="!agents.length" class="empty-agents">Your agents will show up here.</p>
      </div>
      <div class="account"><div class="user-avatar">{{ user.email?.slice(0, 1).toUpperCase() }}</div><div class="account-copy"><strong>{{ user.email }}</strong><small>{{ user.role }}</small></div><button class="icon-button" title="Sign out" @click="signOut">↗</button></div>
    </aside>

    <section class="chat-area">
      <header class="topbar">
        <div class="crumb"><span>Agents</span><b>/</b><strong>{{ selectedAgent ? `Agent ${selectedAgent}` : 'New conversation' }}</strong></div>
        <div class="top-actions"><span class="secure"><span></span> PRIVATE WORKSPACE</span><button v-if="selectedAgent" class="icon-button delete-button" title="Remove agent" :disabled="busy" @click="deleteAgent">⌫</button></div>
      </header>

      <div class="conversation" :class="{ 'has-messages': messages.length }">
        <template v-if="messages.length">
          <div v-for="(message, index) in messages" :key="index" class="message-row" :class="message.role">
            <div class="message-avatar">{{ message.role === 'user' ? user.email?.slice(0, 1).toUpperCase() : 'C' }}</div>
            <div class="message-body"><div class="message-author">{{ message.role === 'user' ? 'You' : `Agent ${selectedAgent}` }}</div><p>{{ message.text }}</p></div>
          </div>
          <div v-if="busy" class="thinking"><span></span><span></span><span></span> Thinking through it…</div>
        </template>
        <div v-else class="welcome">
          <div class="welcome-icon"><span>✳</span></div>
          <div class="eyebrow">{{ selectedAgent ? `YOUR AGENT · ${selectedAgent.toUpperCase()}` : 'YOUR AI WORKSPACE' }}</div>
          <h1>{{ selectedAgent ? 'What are we working on?' : 'Make room for a<br /><em>brilliant idea.</em>' }}</h1>
          <p>{{ selectedAgent ? 'Share a goal, ask a question, or give your agent a task.' : 'Start a conversation with an agent. It will have its own dedicated workspace.' }}</p>
          <button v-if="!selectedAgent" class="button primary" :disabled="busy" @click="createAgent">＋ Create your first agent</button>
          <div v-else class="suggestions"><button @click="prompt = 'Help me plan my next project'">✳ &nbsp; Help me plan a project</button><button @click="prompt = 'What can you help me with?'">↗ &nbsp; Explore what you can do</button></div>
        </div>
      </div>

      <div class="composer-wrap">
        <p v-if="error" class="error composer-error">{{ error }}</p>
        <form class="composer" @submit.prevent="sendMessage"><textarea v-model="prompt" :disabled="busy" placeholder="Message your agent…" rows="1" @keydown.enter.exact.prevent="sendMessage"></textarea><div class="composer-foot"><span><kbd>↵</kbd> to send <span class="hint-separator">·</span> Your agent runs in its own container</span><button type="submit" class="send-button" :disabled="busy || !prompt.trim()" aria-label="Send message">↑</button></div></form>
        <div class="disclaimer">Codex can make mistakes. Review important work before you ship it.</div>
      </div>
    </section>
  </main>
</template>
