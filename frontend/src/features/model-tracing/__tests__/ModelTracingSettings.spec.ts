import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import ModelTracingSettings from '../ModelTracingSettings.vue'

const { getConfig, updateConfig } = vi.hoisted(() => ({
  getConfig: vi.fn(),
  updateConfig: vi.fn(),
}))

vi.mock('../api', () => ({
  getConfig,
  updateConfig,
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

const runtimeConfig = {
  configured: true,
  enabled: true,
  endpoint: 'https://langfuse.example.com/api/public/otel',
  public_key: 'pk-live',
  has_secret: true,
  prompt_max_bytes: 1048576,
  response_max_bytes: 2097152,
  media_max_bytes: 524288,
  capture_media_content: false,
  source: 'runtime' as const,
  config_version: 7,
  updated_at: '2026-07-21T08:00:00Z',
  updated_by: 42,
}

describe('ModelTracingSettings', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    getConfig.mockResolvedValue(runtimeConfig)
    updateConfig.mockImplementation(async payload => ({
      ...runtimeConfig,
      ...payload,
      has_secret: payload.secret_key === '' ? false : runtimeConfig.has_secret,
      config_version: runtimeConfig.config_version + 1,
    }))
  })

  it('loads the public configuration and preserves an untouched secret', async () => {
    const wrapper = mount(ModelTracingSettings)
    await flushPromises()

    expect(wrapper.text()).not.toContain('deployment-secret')
    expect(wrapper.get<HTMLInputElement>('[data-testid="model-tracing-endpoint"]').element.value)
      .toBe(runtimeConfig.endpoint)
    expect(wrapper.get<HTMLInputElement>('[data-testid="model-tracing-secret"]').element.value)
      .toBe('')

    await wrapper.get('[data-testid="model-tracing-save"]').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledOnce()
    expect(updateConfig).toHaveBeenCalledWith({
      expected_config_version: 7,
      enabled: true,
      endpoint: runtimeConfig.endpoint,
      public_key: runtimeConfig.public_key,
      prompt_max_bytes: 1048576,
      response_max_bytes: 2097152,
      media_max_bytes: 524288,
      capture_media_content: false,
    })
  })

  it('replaces the secret only when an administrator enters a new value', async () => {
    const wrapper = mount(ModelTracingSettings)
    await flushPromises()

    await wrapper.get('[data-testid="model-tracing-secret"]').setValue('replacement-secret')
    await wrapper.get('[data-testid="model-tracing-save"]').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      expected_config_version: 7,
      secret_key: 'replacement-secret',
    }))
  })

  it('sends an explicit empty secret when clear is selected', async () => {
    const wrapper = mount(ModelTracingSettings)
    await flushPromises()

    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('[data-testid="model-tracing-clear-secret"]').setValue(true)
    await wrapper.get('[data-testid="model-tracing-save"]').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      expected_config_version: 7,
      secret_key: '',
    }))
  })
})
