import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, put, del } = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), del: vi.fn() }))

vi.mock('@/api/client', () => ({ apiClient: { get, put, delete: del } }))

import {
  deleteRouteFailoverPolicy,
  getRouteFailoverPolicy,
  saveRouteFailoverPolicy
} from '@/api/admin/groups'

describe('admin group route failover API', () => {
  beforeEach(() => {
    get.mockReset()
    put.mockReset()
    del.mockReset()
  })

  it('loads the policy for a source group', async () => {
    const policy = { id: 3, source_group_id: 42, enabled: true, targets: [] }
    get.mockResolvedValue({ data: policy })

    await expect(getRouteFailoverPolicy(42)).resolves.toEqual(policy)
    expect(get).toHaveBeenCalledWith('/admin/groups/42/route-failover')
  })

  it('saves the policy payload unchanged', async () => {
    const payload = {
      enabled: true,
      max_attempts: 3,
      failure_threshold: 5,
      success_threshold: 2,
      window_seconds: 60,
      open_cooldown_seconds: 60,
      half_open_lease_seconds: 15,
      targets: [{
        id: 19,
        target_group_id: 7,
        priority: 1,
        enabled: true,
        model_mapping: { 'claude-sonnet-4': 'claude-sonnet-4-5' }
      }]
    }
    put.mockResolvedValue({ data: { id: 4, source_group_id: 42, ...payload } })

    await saveRouteFailoverPolicy(42, payload)

    expect(put).toHaveBeenCalledWith('/admin/groups/42/route-failover', payload)
  })

  it('deletes the policy', async () => {
    del.mockResolvedValue({ data: { message: 'deleted' } })

    await expect(deleteRouteFailoverPolicy(42)).resolves.toEqual({ message: 'deleted' })
    expect(del).toHaveBeenCalledWith('/admin/groups/42/route-failover')
  })
})
