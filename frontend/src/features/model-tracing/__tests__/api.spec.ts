import { describe, expect, it, vi } from 'vitest'

import { getConfig, updateConfig } from '../api'

const { get, put } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
}))

vi.mock('@/api/client', () => ({
  apiClient: { get, put },
}))

const config = {
  configured: false,
  enabled: false,
  endpoint: '',
  public_key: '',
  has_secret: false,
  prompt_max_bytes: 1048576,
  response_max_bytes: 1048576,
  media_max_bytes: 1048576,
  capture_media_content: false,
  source: 'disabled' as const,
  config_version: 0,
}

describe('model tracing admin API', () => {
  it('loads the public configuration from the admin endpoint', async () => {
    get.mockResolvedValueOnce({ data: config })

    await expect(getConfig()).resolves.toEqual(config)
    expect(get).toHaveBeenCalledWith('/admin/model-tracing/config')
  })

  it('updates the configuration with optimistic concurrency', async () => {
    const payload = {
      expected_config_version: 0,
      enabled: false,
      endpoint: '',
      public_key: '',
      prompt_max_bytes: 1048576,
      response_max_bytes: 1048576,
      media_max_bytes: 1048576,
      capture_media_content: false,
    }
    put.mockResolvedValueOnce({ data: { ...config, configured: true, config_version: 1 } })

    await updateConfig(payload)

    expect(put).toHaveBeenCalledWith('/admin/model-tracing/config', payload)
  })
})
