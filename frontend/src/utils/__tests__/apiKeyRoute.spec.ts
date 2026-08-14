import { describe, expect, it } from 'vitest'
import {
  MAX_API_KEY_FALLBACK_GROUPS,
  appendFallbackGroupId,
  moveFallbackGroup,
  sanitizeFallbackGroupIds
} from '@/utils/apiKeyRoute'

describe('API key fallback group selection', () => {
  it('keeps unique fallback groups in order, excludes the primary group, and caps at five', () => {
    expect(
      sanitizeFallbackGroupIds([2, 3, 2, 1, 4, 5, 6, 7], 1)
    ).toEqual([2, 3, 4, 5, 6])
    expect(MAX_API_KEY_FALLBACK_GROUPS).toBe(5)
  })

  it('does not append a duplicate, primary, or sixth fallback group', () => {
    expect(appendFallbackGroupId([2, 3], 2, 1)).toEqual([2, 3])
    expect(appendFallbackGroupId([2, 3], 1, 1)).toEqual([2, 3])
    expect(appendFallbackGroupId([2, 3, 4, 5, 6], 7, 1)).toEqual([2, 3, 4, 5, 6])
  })

  it('moves a fallback group without changing the other priorities', () => {
    expect(moveFallbackGroup([2, 3, 4], 2, 'down')).toEqual([3, 2, 4])
    expect(moveFallbackGroup([2, 3, 4], 1, 'up')).toEqual([2, 3, 4])
    expect(moveFallbackGroup([2, 3, 4], 4, 'down')).toEqual([2, 3, 4])
  })
})
