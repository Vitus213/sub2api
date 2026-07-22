import { apiClient } from '@/api/client'

import type { ModelTracingConfig, UpdateModelTracingConfig } from './types'

const CONFIG_ENDPOINT = '/admin/model-tracing/config'

export async function getConfig(): Promise<ModelTracingConfig> {
  const { data } = await apiClient.get<ModelTracingConfig>(CONFIG_ENDPOINT)
  return data
}

export async function updateConfig(payload: UpdateModelTracingConfig): Promise<ModelTracingConfig> {
  const { data } = await apiClient.put<ModelTracingConfig>(CONFIG_ENDPOINT, payload)
  return data
}
