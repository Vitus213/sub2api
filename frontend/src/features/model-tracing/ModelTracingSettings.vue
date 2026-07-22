<template>
  <section class="card" data-testid="model-tracing-settings">
    <header class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
            {{ t('admin.modelTracing.title') }}
          </h2>
          <p class="mt-1 max-w-3xl text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.modelTracing.description') }}
          </p>
        </div>
        <div v-if="config" class="text-right text-xs text-gray-500 dark:text-gray-400">
          <p>{{ t(`admin.modelTracing.source${sourceLabelSuffix}`) }}</p>
          <p>{{ t('admin.modelTracing.version', { version: config.config_version }) }}</p>
          <p v-if="config.updated_by">
            {{ t('admin.modelTracing.updatedBy', { id: config.updated_by }) }}
          </p>
        </div>
      </div>
    </header>

    <div v-if="loading" class="flex items-center gap-2 p-6 text-sm text-gray-500 dark:text-gray-400">
      <span class="h-4 w-4 animate-spin rounded-full border-2 border-gray-300 border-t-primary-600" />
      {{ t('common.loading') }}
    </div>

    <div v-else-if="loadError" class="space-y-3 p-6">
      <p class="text-sm text-red-600 dark:text-red-400">{{ loadError }}</p>
      <button type="button" class="btn btn-secondary" @click="loadConfig">
        {{ t('admin.modelTracing.reload') }}
      </button>
    </div>

    <div v-else-if="draft" class="space-y-6 p-6">
      <div class="flex items-center justify-between gap-6">
        <div>
          <label class="font-medium text-gray-900 dark:text-white">
            {{ t('admin.modelTracing.enabled') }}
          </label>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.modelTracing.enabledHint') }}
          </p>
        </div>
        <Toggle v-model="draft.enabled" />
      </div>

      <div class="grid gap-5 md:grid-cols-2">
        <label class="block md:col-span-2">
          <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.modelTracing.endpoint') }}
          </span>
          <input
            v-model.trim="draft.endpoint"
            data-testid="model-tracing-endpoint"
            type="url"
            autocomplete="url"
            class="input mt-1 w-full"
            placeholder="https://langfuse.example.com/api/public/otel"
          />
          <span class="mt-1 block text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.modelTracing.endpointHint') }}
          </span>
        </label>

        <label class="block">
          <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.modelTracing.publicKey') }}
          </span>
          <input v-model.trim="draft.publicKey" type="text" autocomplete="off" class="input mt-1 w-full" />
        </label>

        <label class="block">
          <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.modelTracing.secretKey') }}
          </span>
          <input
            v-model="draft.secretKey"
            data-testid="model-tracing-secret"
            type="password"
            autocomplete="new-password"
            class="input mt-1 w-full"
            :disabled="draft.clearSecret"
          />
          <span class="mt-1 block text-xs text-gray-500 dark:text-gray-400">
            {{ t(config?.has_secret ? 'admin.modelTracing.secretConfigured' : 'admin.modelTracing.secretNotConfigured') }}
          </span>
        </label>
      </div>

      <label v-if="config?.has_secret" class="flex items-start gap-3 rounded-lg border border-red-200 bg-red-50 p-3 dark:border-red-900/60 dark:bg-red-950/20">
        <input
          v-model="draft.clearSecret"
          data-testid="model-tracing-clear-secret"
          type="checkbox"
          class="mt-1 h-4 w-4 rounded border-gray-300 text-red-600 focus:ring-red-500"
        />
        <span>
          <span class="block text-sm font-medium text-red-700 dark:text-red-300">
            {{ t('admin.modelTracing.clearSecret') }}
          </span>
          <span class="mt-1 block text-xs text-red-600 dark:text-red-400">
            {{ t('admin.modelTracing.clearSecretHint') }}
          </span>
        </span>
      </label>

      <fieldset class="space-y-4">
        <legend class="text-sm font-semibold text-gray-900 dark:text-white">
          {{ t('admin.modelTracing.contentLimits') }}
        </legend>
        <div class="grid gap-4 md:grid-cols-3">
          <label v-for="field in byteLimitFields" :key="field.key" class="block">
            <span class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t(field.label) }}</span>
            <input
              v-model.number="draft[field.key]"
              type="number"
              min="1"
			  :max="MAX_CAPTURE_BYTES"
              step="1"
              class="input mt-1 w-full"
            />
          </label>
        </div>
      </fieldset>

      <div class="flex items-center justify-between gap-6">
        <div>
          <label class="font-medium text-gray-900 dark:text-white">
            {{ t('admin.modelTracing.captureMedia') }}
          </label>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.modelTracing.captureMediaHint') }}
          </p>
        </div>
        <Toggle v-model="draft.captureMediaContent" />
      </div>

      <p v-if="message" :class="messageKind === 'error' ? 'text-red-600 dark:text-red-400' : 'text-green-600 dark:text-green-400'" class="text-sm">
        {{ message }}
      </p>

      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="loadConfig">
          {{ t('admin.modelTracing.reload') }}
        </button>
        <button
          type="button"
          data-testid="model-tracing-save"
          class="btn btn-primary"
          :disabled="saving"
          @click="saveConfig"
        >
          {{ saving ? t('common.saving') : t('admin.modelTracing.save') }}
        </button>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import Toggle from '@/components/common/Toggle.vue'
import { extractApiErrorMessage } from '@/utils/apiError'

import { getConfig, updateConfig } from './api'
import type { ModelTracingConfig, UpdateModelTracingConfig } from './types'

const MAX_CAPTURE_BYTES = 8 * 1024 * 1024

type ByteLimitKey = 'promptMaxBytes' | 'responseMaxBytes' | 'mediaMaxBytes'

interface ConfigDraft {
  enabled: boolean
  endpoint: string
  publicKey: string
  secretKey: string
  clearSecret: boolean
  promptMaxBytes: number
  responseMaxBytes: number
  mediaMaxBytes: number
  captureMediaContent: boolean
}

const { t } = useI18n()
const loading = ref(true)
const saving = ref(false)
const loadError = ref('')
const message = ref('')
const messageKind = ref<'success' | 'error'>('success')
const config = ref<ModelTracingConfig | null>(null)
const draft = ref<ConfigDraft | null>(null)

const byteLimitFields: Array<{ key: ByteLimitKey; label: string }> = [
  { key: 'promptMaxBytes', label: 'admin.modelTracing.promptMaxBytes' },
  { key: 'responseMaxBytes', label: 'admin.modelTracing.responseMaxBytes' },
  { key: 'mediaMaxBytes', label: 'admin.modelTracing.mediaMaxBytes' },
]

const sourceLabelSuffix = computed(() => {
  if (config.value?.source === 'deployment') return 'Deployment'
  if (config.value?.source === 'runtime') return 'Runtime'
  return 'Disabled'
})

function applyConfig(value: ModelTracingConfig) {
  config.value = value
  draft.value = {
    enabled: value.enabled,
    endpoint: value.endpoint,
    publicKey: value.public_key,
    secretKey: '',
    clearSecret: false,
    promptMaxBytes: value.prompt_max_bytes,
    responseMaxBytes: value.response_max_bytes,
    mediaMaxBytes: value.media_max_bytes,
    captureMediaContent: value.capture_media_content,
  }
}

async function loadConfig() {
  loading.value = true
  loadError.value = ''
  message.value = ''
  try {
    applyConfig(await getConfig())
  } catch (error) {
    loadError.value = extractApiErrorMessage(error, t('admin.modelTracing.loadFailed'))
  } finally {
    loading.value = false
  }
}

function hasUsableSecret(value: ConfigDraft): boolean {
  if (value.clearSecret) return false
  return value.secretKey.length > 0 || Boolean(config.value?.has_secret)
}

function positiveInteger(value: number): number {
  return Number.isFinite(value) && value > 0 ? Math.min(MAX_CAPTURE_BYTES, Math.floor(value)) : 1
}

async function saveConfig() {
  if (!draft.value || !config.value || saving.value) return
  const value = draft.value
  message.value = ''

  if (value.enabled && (!value.endpoint || !value.publicKey || !hasUsableSecret(value))) {
    messageKind.value = 'error'
    message.value = t('admin.modelTracing.requiredWhenEnabled')
    return
  }

  const payload: UpdateModelTracingConfig = {
    expected_config_version: config.value.config_version,
    enabled: value.enabled,
    endpoint: value.endpoint,
    public_key: value.publicKey,
    prompt_max_bytes: positiveInteger(value.promptMaxBytes),
    response_max_bytes: positiveInteger(value.responseMaxBytes),
    media_max_bytes: positiveInteger(value.mediaMaxBytes),
    capture_media_content: value.captureMediaContent,
  }
  if (value.clearSecret) payload.secret_key = ''
  else if (value.secretKey.length > 0) payload.secret_key = value.secretKey

  saving.value = true
  try {
    applyConfig(await updateConfig(payload))
    messageKind.value = 'success'
    message.value = t('admin.modelTracing.saved')
  } catch (error) {
    messageKind.value = 'error'
    message.value = extractApiErrorMessage(error, t('admin.modelTracing.saveFailed'))
  } finally {
    saving.value = false
  }
}

onMounted(loadConfig)
</script>
