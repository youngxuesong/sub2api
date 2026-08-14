export const MAX_API_KEY_FALLBACK_GROUPS = 5

export type FallbackMoveDirection = 'up' | 'down'

export function sanitizeFallbackGroupIds(
  groupIds: number[],
  primaryGroupId: number | null | undefined
): number[] {
  const seen = new Set<number>()
  const sanitized: number[] = []

  for (const groupId of groupIds) {
    if (!Number.isInteger(groupId) || groupId <= 0 || groupId === primaryGroupId || seen.has(groupId)) {
      continue
    }
    seen.add(groupId)
    sanitized.push(groupId)
    if (sanitized.length === MAX_API_KEY_FALLBACK_GROUPS) {
      break
    }
  }

  return sanitized
}

export function appendFallbackGroupId(
  groupIds: number[],
  groupId: number,
  primaryGroupId: number | null | undefined
): number[] {
  return sanitizeFallbackGroupIds([...groupIds, groupId], primaryGroupId)
}

export function moveFallbackGroup(
  groupIds: number[],
  groupId: number,
  direction: FallbackMoveDirection
): number[] {
  const index = groupIds.indexOf(groupId)
  const nextIndex = direction === 'up' ? index - 1 : index + 1
  if (index < 0 || nextIndex < 0 || nextIndex >= groupIds.length) {
    return [...groupIds]
  }

  const reordered = [...groupIds]
  reordered[index] = reordered[nextIndex]
  reordered[nextIndex] = groupId
  return reordered
}
