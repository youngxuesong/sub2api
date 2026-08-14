<template>
  <section class="space-y-3" data-testid="fallback-group-selector">
    <div>
      <label class="input-label" for="fallback-group-select">
        {{ t('keys.fallbackGroupsLabel') }}
      </label>
      <p class="input-hint">{{ t('keys.fallbackGroupsHint', { count: MAX_API_KEY_FALLBACK_GROUPS }) }}</p>
    </div>

    <Select
      id="fallback-group-select"
      data-testid="fallback-group-select"
      :model-value="selectedGroupToAdd"
      :options="availableGroupOptions"
      :placeholder="modelValue.length >= MAX_API_KEY_FALLBACK_GROUPS
        ? t('keys.fallbackGroupsLimitReached')
        : t('keys.addFallbackGroup')"
      :searchable="true"
      :search-placeholder="t('keys.searchFallbackGroup')"
      :disabled="modelValue.length >= MAX_API_KEY_FALLBACK_GROUPS"
      :aria-label="t('keys.addFallbackGroup')"
      @update:model-value="handleAdd"
    >
      <template #option="{ option, selected }">
        <GroupOptionItem
          :name="(option as unknown as FallbackGroupOption).label"
          :platform="(option as unknown as FallbackGroupOption).platform"
          :subscription-type="(option as unknown as FallbackGroupOption).subscriptionType"
          :rate-multiplier="(option as unknown as FallbackGroupOption).rate"
          :user-rate-multiplier="(option as unknown as FallbackGroupOption).userRate"
          :peak-rate-enabled="(option as unknown as FallbackGroupOption).peakRateEnabled"
          :peak-start="(option as unknown as FallbackGroupOption).peakStart"
          :peak-end="(option as unknown as FallbackGroupOption).peakEnd"
          :peak-rate-multiplier="(option as unknown as FallbackGroupOption).peakRateMultiplier"
          :description="(option as unknown as FallbackGroupOption).description"
          :selected="selected"
        />
      </template>
    </Select>

    <ol
      v-if="selectedGroups.length > 0"
      class="space-y-2"
      :aria-label="t('keys.fallbackGroupsOrder')"
      data-testid="fallback-group-list"
    >
      <li
        v-for="(group, index) in selectedGroups"
        :key="group.id"
        class="flex items-center gap-2 rounded-lg border border-gray-200 px-3 py-2 dark:border-dark-600"
      >
        <span class="w-5 text-center text-xs font-medium text-gray-500 dark:text-gray-400">{{ index + 1 }}</span>
        <span class="min-w-0 flex-1 truncate text-sm text-gray-900 dark:text-white">{{ group.name }}</span>
        <button
          type="button"
          class="btn btn-ghost h-8 w-8 flex-shrink-0 rounded-lg p-0"
          :disabled="index === 0"
          :aria-label="t('keys.moveFallbackUp', { name: group.name })"
          :title="t('keys.moveFallbackUp', { name: group.name })"
          :data-testid="`fallback-move-up-${group.id}`"
          @click="moveGroup(group.id, 'up')"
        >
          <Icon name="arrowUp" size="sm" />
        </button>
        <button
          type="button"
          class="btn btn-ghost h-8 w-8 flex-shrink-0 rounded-lg p-0"
          :disabled="index === selectedGroups.length - 1"
          :aria-label="t('keys.moveFallbackDown', { name: group.name })"
          :title="t('keys.moveFallbackDown', { name: group.name })"
          :data-testid="`fallback-move-down-${group.id}`"
          @click="moveGroup(group.id, 'down')"
        >
          <Icon name="arrowDown" size="sm" />
        </button>
        <button
          type="button"
          class="btn btn-ghost h-8 w-8 flex-shrink-0 rounded-lg p-0 text-red-500 hover:text-red-600"
          :aria-label="t('keys.removeFallbackGroup', { name: group.name })"
          :title="t('keys.removeFallbackGroup', { name: group.name })"
          :data-testid="`fallback-remove-${group.id}`"
          @click="removeGroup(group.id)"
        >
          <Icon name="trash" size="sm" />
        </button>
      </li>
    </ol>

    <div v-if="modelValue.length > 0" class="rounded-lg border border-amber-200 bg-amber-50 p-3 dark:border-amber-800/60 dark:bg-amber-900/20">
      <div class="flex gap-2">
        <Icon name="exclamationTriangle" size="sm" class="mt-0.5 flex-shrink-0 text-amber-600 dark:text-amber-400" />
        <div class="space-y-1 text-sm">
          <p class="font-medium text-amber-800 dark:text-amber-300">{{ t('keys.fallbackRiskTitle') }}</p>
          <p class="text-amber-700 dark:text-amber-400">{{ t('keys.fallbackRiskDescription') }}</p>
          <label class="flex cursor-pointer items-start gap-2 pt-1 text-amber-900 dark:text-amber-200">
            <input
              id="fallback-risk-checkbox"
              data-testid="fallback-risk-checkbox"
              type="checkbox"
              class="mt-0.5 rounded border-amber-400 text-primary-600 focus:ring-primary-500"
              :checked="riskAcknowledged"
              @change="handleRiskChange"
            />
            <span>{{ t('keys.fallbackRiskAcknowledge') }}</span>
          </label>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import GroupOptionItem from '@/components/common/GroupOptionItem.vue'
import Select from '@/components/common/Select.vue'
import type {
  ApiKeyFallbackGroup,
  Group,
  GroupPlatform,
  SubscriptionType
} from '@/types'
import {
  MAX_API_KEY_FALLBACK_GROUPS,
  appendFallbackGroupId,
  moveFallbackGroup,
  type FallbackMoveDirection
} from '@/utils/apiKeyRoute'

const { t } = useI18n()

interface FallbackGroupOption {
  value: number
  label: string
  description: string | null
  rate: number
  userRate: number | null
  peakRateEnabled: boolean
  peakStart: string
  peakEnd: string
  peakRateMultiplier: number
  subscriptionType: SubscriptionType
  platform: GroupPlatform
}

const props = withDefaults(defineProps<{
  groups: Group[]
  fallbackGroups?: ApiKeyFallbackGroup[]
  userGroupRates?: Record<number, number>
  primaryGroupId: number | null
  modelValue: number[]
  riskAcknowledged: boolean
}>(), {
  fallbackGroups: () => [],
  userGroupRates: () => ({})
})

const emit = defineEmits<{
  (event: 'update:modelValue', value: number[]): void
  (event: 'update:riskAcknowledged', value: boolean): void
}>()

const selectedGroupToAdd = ref<number | null>(null)

const availableGroupOptions = computed(() => props.groups
  .filter((group) => group.id !== props.primaryGroupId && !props.modelValue.includes(group.id))
  .map((group) => ({
    value: group.id,
    label: group.name,
    description: group.description,
    rate: group.rate_multiplier,
    userRate: props.userGroupRates[group.id] ?? null,
    peakRateEnabled: group.peak_rate_enabled,
    peakStart: group.peak_start,
    peakEnd: group.peak_end,
    peakRateMultiplier: group.peak_rate_multiplier,
    subscriptionType: group.subscription_type,
    platform: group.platform
  })))

const groupNameById = computed(() => {
  const names = new Map<number, string>()
  for (const group of props.fallbackGroups) names.set(group.id, group.name)
  for (const group of props.groups) names.set(group.id, group.name)
  return names
})

const selectedGroups = computed(() => props.modelValue.map((id) => ({
  id,
  name: groupNameById.value.get(id) || t('keys.unknownFallbackGroup', { id })
})))

const resetRiskAcknowledgement = () => emit('update:riskAcknowledged', false)

const handleAdd = (value: string | number | boolean | null) => {
  if (value === null || typeof value === 'boolean') return
  const groupId = Number(value)
  if (!Number.isInteger(groupId)) return

  const next = appendFallbackGroupId(props.modelValue, groupId, props.primaryGroupId)
  if (next.length === props.modelValue.length) return
  emit('update:modelValue', next)
  resetRiskAcknowledgement()
  selectedGroupToAdd.value = null
}

const removeGroup = (groupId: number) => {
  emit('update:modelValue', props.modelValue.filter((id) => id !== groupId))
  resetRiskAcknowledgement()
}

const moveGroup = (groupId: number, direction: FallbackMoveDirection) => {
  emit('update:modelValue', moveFallbackGroup(props.modelValue, groupId, direction))
  resetRiskAcknowledgement()
}

const handleRiskChange = (event: Event) => {
  emit('update:riskAcknowledged', (event.target as HTMLInputElement).checked)
}
</script>
