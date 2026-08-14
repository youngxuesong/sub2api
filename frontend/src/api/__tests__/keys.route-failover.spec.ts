import { beforeEach, describe, expect, it, vi } from 'vitest'

const { mockPost, mockPut } = vi.hoisted(() => ({
  mockPost: vi.fn(),
  mockPut: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: {
    post: mockPost,
    put: mockPut
  }
}))

import { keysAPI } from '@/api/keys'

describe('API key route failover payloads', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockPost.mockResolvedValue({ data: { id: 1 } })
    mockPut.mockResolvedValue({ data: { id: 1 } })
  })

  it('sends ordered fallback groups and explicit risk acknowledgement on create', async () => {
    await keysAPI.create(
      'Route key',
      1,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      [9, 8],
      true
    )

    expect(mockPost).toHaveBeenCalledWith('/keys', {
      name: 'Route key',
      group_id: 1,
      fallback_group_ids: [9, 8],
      failover_risk_acknowledged: true
    })
  })

  it('sends an empty fallback list to clear routing on edit', async () => {
    await keysAPI.update(7, {
      fallback_group_ids: [],
      failover_risk_acknowledged: false
    })

    expect(mockPut).toHaveBeenCalledWith('/keys/7', {
      fallback_group_ids: [],
      failover_risk_acknowledged: false
    })
  })

  it('preserves fallback priority and acknowledgement on edit', async () => {
    await keysAPI.update(7, {
      fallback_group_ids: [9, 8],
      failover_risk_acknowledged: true
    })

    expect(mockPut).toHaveBeenCalledWith('/keys/7', {
      fallback_group_ids: [9, 8],
      failover_risk_acknowledged: true
    })
  })
})
