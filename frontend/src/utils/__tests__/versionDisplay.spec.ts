import { describe, expect, it } from 'vitest'

import { formatVersionLabel } from '@/utils/versionDisplay'

describe('formatVersionLabel', () => {
  it('keeps a stable semantic version compact', () => {
    expect(formatVersionLabel('0.1.176')).toBe('v0.1.176')
    expect(formatVersionLabel('v0.1.176')).toBe('v0.1.176')
  })

  it('hides long build metadata from the primary UI label', () => {
    expect(formatVersionLabel('0.1.176-merged-0.1.176-route-failover-20260814')).toBe(
      'v0.1.176'
    )
  })

  it('trims and bounds non-semantic development labels', () => {
    expect(formatVersionLabel('  dev-local  ')).toBe('dev-local')
    expect(formatVersionLabel('custom-development-build-20260814')).toBe('custom-develop...')
  })

  it('returns an empty label when no version is available', () => {
    expect(formatVersionLabel('')).toBe('')
    expect(formatVersionLabel(undefined)).toBe('')
  })
})
