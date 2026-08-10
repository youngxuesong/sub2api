<template>
  <BaseDialog
    :show="show"
    :title="t('admin.groups.routeFailover.titleWithGroup', { name: group?.name || '' })"
    width="wide"
    @close="handleClose"
  >
    <div v-if="loading" class="flex min-h-48 items-center justify-center">
      <Icon name="refresh" size="lg" class="animate-spin text-primary-500" />
    </div>

    <div v-else-if="loadFailed" data-testid="failover-load-error" class="py-12 text-center text-sm text-red-600 dark:text-red-400">
      {{ t('admin.groups.routeFailover.failedToLoad') }}
    </div>

    <form v-else-if="group" class="space-y-5" @submit.prevent="handleSave">
      <div class="flex flex-wrap items-center justify-between gap-3 border-b border-gray-200 pb-4 dark:border-dark-600">
        <div class="flex items-center gap-2 text-sm text-gray-600 dark:text-gray-300">
          <PlatformIcon :platform="group.platform" size="sm" />
          <span class="font-medium text-gray-900 dark:text-white">{{ group.name }}</span>
          <span class="text-gray-400">{{ group.subscription_type }}</span>
        </div>
        <label class="inline-flex cursor-pointer items-center gap-2 text-sm font-medium text-gray-700 dark:text-gray-200">
          <input
            v-model="form.enabled"
            data-testid="failover-enabled"
            type="checkbox"
            class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
          />
          {{ t('admin.groups.routeFailover.enabled') }}
        </label>
      </div>

      <div>
        <h4 class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">
          {{ t('admin.groups.routeFailover.circuitSettings') }}
        </h4>
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
          <label v-for="field in numericFields" :key="field.key" class="block">
            <span class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-300">
              {{ t(`admin.groups.routeFailover.${field.label}`) }}
            </span>
            <input
              v-model.number="form[field.key]"
              :data-testid="field.testId"
              type="number"
              :min="field.min"
              :max="field.max"
              step="1"
              class="input w-full"
            />
          </label>
        </div>
        <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.groups.routeFailover.circuitHint') }}
        </p>
      </div>

      <div class="border-t border-gray-200 pt-4 dark:border-dark-600">
        <div class="mb-3 flex flex-wrap items-end justify-between gap-3">
          <div>
            <h4 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t('admin.groups.routeFailover.targets') }}
            </h4>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.groups.routeFailover.targetsHint') }}
            </p>
          </div>
          <div class="flex min-w-0 flex-1 items-center justify-end gap-2 sm:max-w-md">
            <select
              v-model="newTargetGroupId"
              data-testid="failover-new-target"
              class="input min-w-0 flex-1"
              :aria-label="t('admin.groups.routeFailover.targetGroup')"
              :disabled="availableTargetOptions.length === 0 || form.targets.length >= 9"
            >
              <option value="">{{ t('admin.groups.routeFailover.selectTarget') }}</option>
              <option v-for="candidate in availableTargetOptions" :key="candidate.id" :value="String(candidate.id)">
                {{ candidate.name }} (#{{ candidate.id }})
              </option>
            </select>
            <button
              data-testid="failover-add-target"
              type="button"
              class="btn btn-primary shrink-0"
              :disabled="!newTargetGroupId || form.targets.length >= 9"
              @click="addTarget"
            >
              <Icon name="plus" size="sm" />
              <span>{{ t('common.add') }}</span>
            </button>
          </div>
        </div>

        <div v-if="form.targets.length === 0" class="border-y border-dashed border-gray-200 py-8 text-center text-sm text-gray-400 dark:border-dark-600">
          {{ t('admin.groups.routeFailover.noTargets') }}
        </div>

        <div v-else class="divide-y divide-gray-200 border-y border-gray-200 dark:divide-dark-600 dark:border-dark-600">
          <div v-for="(target, targetIndex) in form.targets" :key="target.key" class="py-4">
            <div class="flex flex-wrap items-center gap-3">
              <span class="flex h-7 w-7 shrink-0 items-center justify-center rounded bg-gray-100 text-xs font-semibold text-gray-600 dark:bg-dark-700 dark:text-gray-300">
                {{ targetIndex + 1 }}
              </span>
              <div class="min-w-0 flex-1">
                <div class="truncate text-sm font-medium text-gray-900 dark:text-white">
                  {{ targetGroupName(target.targetGroupId) }}
                </div>
                <div class="text-xs text-gray-400">#{{ target.targetGroupId }}</div>
              </div>
              <label class="inline-flex items-center gap-1.5 text-xs text-gray-600 dark:text-gray-300">
                <input v-model="target.enabled" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600" />
                {{ t('admin.groups.routeFailover.targetEnabled') }}
              </label>
              <div class="flex items-center gap-1">
                <button type="button" class="rounded p-1.5 text-gray-400 hover:bg-gray-100 hover:text-gray-700 disabled:opacity-30 dark:hover:bg-dark-700 dark:hover:text-gray-200" :disabled="targetIndex === 0" :title="t('admin.groups.routeFailover.moveUp')" @click="moveTarget(targetIndex, -1)">
                  <Icon name="arrowUp" size="sm" />
                </button>
                <button type="button" class="rounded p-1.5 text-gray-400 hover:bg-gray-100 hover:text-gray-700 disabled:opacity-30 dark:hover:bg-dark-700 dark:hover:text-gray-200" :disabled="targetIndex === form.targets.length - 1" :title="t('admin.groups.routeFailover.moveDown')" @click="moveTarget(targetIndex, 1)">
                  <Icon name="arrowDown" size="sm" />
                </button>
                <button type="button" class="rounded p-1.5 text-gray-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400" :title="t('common.delete')" @click="removeTarget(targetIndex)">
                  <Icon name="trash" size="sm" />
                </button>
              </div>
            </div>

            <div class="ml-0 mt-3 sm:ml-10">
              <div class="mb-2 flex items-center justify-between gap-2">
                <span class="text-xs font-medium text-gray-600 dark:text-gray-300">
                  {{ t('admin.groups.routeFailover.modelMappings') }}
                </span>
                <button
                  :data-testid="`failover-add-mapping-${targetIndex}`"
                  type="button"
                  class="inline-flex items-center gap-1 text-xs font-medium text-primary-600 hover:text-primary-700 dark:text-primary-400"
                  @click="addMapping(targetIndex)"
                >
                  <Icon name="plus" size="sm" />
                  {{ t('admin.groups.routeFailover.addMapping') }}
                </button>
              </div>
              <p v-if="target.mappings.length === 0" class="text-xs text-gray-400">
                {{ t('admin.groups.routeFailover.passThroughModel') }}
              </p>
              <div v-for="(mapping, mappingIndex) in target.mappings" :key="mapping.key" class="mb-2 grid grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)_auto] items-center gap-2">
                <input v-model="mapping.from" :data-testid="`failover-mapping-from-${targetIndex}-${mappingIndex}`" class="input min-w-0" :placeholder="t('admin.groups.routeFailover.sourceModel')" />
                <Icon name="arrowRight" size="sm" class="text-gray-400" />
                <input v-model="mapping.to" :data-testid="`failover-mapping-to-${targetIndex}-${mappingIndex}`" class="input min-w-0" :placeholder="t('admin.groups.routeFailover.targetModel')" />
                <button type="button" class="rounded p-1.5 text-gray-400 hover:text-red-600" :title="t('common.delete')" @click="removeMapping(targetIndex, mappingIndex)">
                  <Icon name="x" size="sm" />
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>

      <p v-if="validationError" role="alert" class="text-sm text-red-600 dark:text-red-400">
        {{ validationError }}
      </p>

      <div class="flex flex-wrap items-center gap-3 border-t border-gray-200 pt-4 dark:border-dark-600">
        <button v-if="hasPolicy" type="button" class="btn text-red-600 hover:bg-red-50 dark:text-red-400 dark:hover:bg-red-900/20" :disabled="saving || deleting" @click="handleDelete">
          <Icon name="trash" size="sm" />
          {{ t('admin.groups.routeFailover.deletePolicy') }}
        </button>
        <div class="ml-auto flex items-center gap-2">
          <button type="button" class="btn" :disabled="saving || deleting" @click="handleClose">
            {{ t('common.cancel') }}
          </button>
          <button data-testid="failover-save" type="submit" class="btn btn-primary" :disabled="saving || deleting">
            <Icon v-if="saving" name="refresh" size="sm" class="animate-spin" />
            {{ t('common.save') }}
          </button>
        </div>
      </div>
    </form>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import type { AdminGroup, RouteFailoverPolicy, RouteFailoverPolicyInput } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'
import BaseDialog from '@/components/common/BaseDialog.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import Icon from '@/components/icons/Icon.vue'

type NumericField = Exclude<keyof RouteFailoverPolicyInput, 'enabled' | 'targets'>
interface MappingRow { key: number; from: string; to: string }
interface TargetRow { key: number; id?: number; targetGroupId: number; enabled: boolean; mappings: MappingRow[] }
interface FailoverForm extends Omit<RouteFailoverPolicyInput, 'targets'> { targets: TargetRow[] }

const props = defineProps<{ show: boolean; group: AdminGroup | null }>()
const emit = defineEmits<{ close: []; success: [] }>()
const { t } = useI18n()
const appStore = useAppStore()

const defaults = (): FailoverForm => ({
  enabled: false,
  max_attempts: 3,
  failure_threshold: 5,
  success_threshold: 2,
  window_seconds: 60,
  open_cooldown_seconds: 60,
  half_open_lease_seconds: 15,
  targets: []
})

const form = reactive<FailoverForm>(defaults())
const loading = ref(false)
const loadFailed = ref(false)
const saving = ref(false)
const deleting = ref(false)
const hasPolicy = ref(false)
const allGroups = ref<AdminGroup[]>([])
const newTargetGroupId = ref('')
const validationError = ref('')
let rowKey = 0
let loadVersion = 0

const numericFields: Array<{ key: NumericField; label: string; testId?: string; min: number; max?: number }> = [
  { key: 'max_attempts', label: 'maxAttempts', testId: 'failover-max-attempts', min: 1, max: 10 },
  { key: 'failure_threshold', label: 'failureThreshold', min: 1 },
  { key: 'success_threshold', label: 'successThreshold', min: 1 },
  { key: 'window_seconds', label: 'windowSeconds', min: 1 },
  { key: 'open_cooldown_seconds', label: 'openCooldownSeconds', min: 1 },
  { key: 'half_open_lease_seconds', label: 'halfOpenLeaseSeconds', min: 1 }
]

const compatibleGroups = computed(() => {
  if (!props.group) return []
  return allGroups.value.filter(candidate =>
    candidate.id !== props.group!.id &&
    candidate.status === 'active' &&
    candidate.platform === props.group!.platform &&
    candidate.subscription_type === props.group!.subscription_type
  )
})

const availableTargetOptions = computed(() => {
  const selected = new Set(form.targets.map(target => target.targetGroupId))
  return compatibleGroups.value.filter(candidate => !selected.has(candidate.id))
})

function resetForm(policy?: RouteFailoverPolicy) {
  Object.assign(form, defaults())
  if (!policy) return
  form.enabled = policy.enabled
  form.max_attempts = policy.max_attempts
  form.failure_threshold = policy.failure_threshold
  form.success_threshold = policy.success_threshold
  form.window_seconds = policy.window_seconds
  form.open_cooldown_seconds = policy.open_cooldown_seconds
  form.half_open_lease_seconds = policy.half_open_lease_seconds
  form.targets = [...policy.targets]
    .sort((a, b) => a.priority - b.priority)
    .map(target => ({
      key: ++rowKey,
      id: target.id,
      targetGroupId: target.target_group_id,
      enabled: target.enabled,
      mappings: Object.entries(target.model_mapping || {}).map(([from, to]) => ({ key: ++rowKey, from, to }))
    }))
}

function isNotFound(error: unknown) {
  if (!error || typeof error !== 'object') return false
  const value = error as { status?: number; code?: string }
  return value.status === 404 || value.code === 'ROUTE_FAILOVER_POLICY_NOT_FOUND'
}

async function load() {
  if (!props.group) return
  const version = ++loadVersion
  loading.value = true
  loadFailed.value = false
  allGroups.value = []
  hasPolicy.value = false
  validationError.value = ''
  newTargetGroupId.value = ''
  resetForm()
  try {
    const policyPromise = adminAPI.groups.getRouteFailoverPolicy(props.group.id).catch(error => {
      if (isNotFound(error)) return undefined
      throw error
    })
    const [groups, policy] = await Promise.all([
      adminAPI.groups.getAllIncludingInactive(),
      policyPromise
    ])
    if (version !== loadVersion) return
    allGroups.value = groups
    hasPolicy.value = Boolean(policy)
    resetForm(policy)
  } catch (error) {
    if (version !== loadVersion) return
    loadFailed.value = true
    appStore.showError(extractApiErrorMessage(error, t('admin.groups.routeFailover.failedToLoad')))
  } finally {
    if (version === loadVersion) loading.value = false
  }
}

function addTarget() {
  const id = Number(newTargetGroupId.value)
  if (!id || form.targets.length >= 9 || !availableTargetOptions.value.some(group => group.id === id)) return
  form.targets.push({ key: ++rowKey, targetGroupId: id, enabled: true, mappings: [] })
  newTargetGroupId.value = ''
}

function removeTarget(index: number) { form.targets.splice(index, 1) }
function moveTarget(index: number, offset: number) {
  const next = index + offset
  if (next < 0 || next >= form.targets.length) return
  const [target] = form.targets.splice(index, 1)
  form.targets.splice(next, 0, target)
}
function addMapping(targetIndex: number) {
  form.targets[targetIndex]?.mappings.push({ key: ++rowKey, from: '', to: '' })
}
function removeMapping(targetIndex: number, mappingIndex: number) {
  form.targets[targetIndex]?.mappings.splice(mappingIndex, 1)
}
function targetGroupName(id: number) { return allGroups.value.find(group => group.id === id)?.name || t('admin.groups.routeFailover.unknownGroup') }

function buildPayload(): RouteFailoverPolicyInput | null {
  validationError.value = ''
  for (const field of numericFields) {
    const value = Number(form[field.key])
    if (!Number.isInteger(value) || value < field.min || (field.max !== undefined && value > field.max)) {
      validationError.value = t('admin.groups.routeFailover.invalidNumbers')
      return null
    }
  }
  if (form.enabled && form.targets.length === 0) {
    validationError.value = t('admin.groups.routeFailover.targetRequired')
    return null
  }
  const selected = new Set<number>()
  const targets = []
  for (const [index, target] of form.targets.entries()) {
    if (selected.has(target.targetGroupId) || !compatibleGroups.value.some(group => group.id === target.targetGroupId)) {
      validationError.value = t('admin.groups.routeFailover.invalidTarget')
      return null
    }
    selected.add(target.targetGroupId)
    const modelMapping: Record<string, string> = {}
    for (const mapping of target.mappings) {
      const from = mapping.from.trim()
      const to = mapping.to.trim()
      if (!from || !to || modelMapping[from]) {
        validationError.value = t('admin.groups.routeFailover.invalidMapping')
        return null
      }
      modelMapping[from] = to
    }
    targets.push({
      ...(target.id ? { id: target.id } : {}),
      target_group_id: target.targetGroupId,
      priority: index + 1,
      enabled: target.enabled,
      model_mapping: modelMapping
    })
  }
  return {
    enabled: form.enabled,
    max_attempts: Number(form.max_attempts),
    failure_threshold: Number(form.failure_threshold),
    success_threshold: Number(form.success_threshold),
    window_seconds: Number(form.window_seconds),
    open_cooldown_seconds: Number(form.open_cooldown_seconds),
    half_open_lease_seconds: Number(form.half_open_lease_seconds),
    targets
  }
}

async function handleSave() {
  if (!props.group || saving.value || loadFailed.value) return
  const payload = buildPayload()
  if (!payload) return
  saving.value = true
  try {
    const saved = await adminAPI.groups.saveRouteFailoverPolicy(props.group.id, payload)
    hasPolicy.value = true
    resetForm(saved)
    appStore.showSuccess(t('admin.groups.routeFailover.saved'))
    emit('success')
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('admin.groups.routeFailover.failedToSave')))
  } finally {
    saving.value = false
  }
}

async function handleDelete() {
  if (!props.group || deleting.value || !window.confirm(t('admin.groups.routeFailover.deleteConfirm'))) return
  deleting.value = true
  try {
    await adminAPI.groups.deleteRouteFailoverPolicy(props.group.id)
    hasPolicy.value = false
    resetForm()
    appStore.showSuccess(t('admin.groups.routeFailover.deleted'))
    emit('success')
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('admin.groups.routeFailover.failedToDelete')))
  } finally {
    deleting.value = false
  }
}

function handleClose() {
  if (saving.value || deleting.value) return
  emit('close')
}

watch(() => [props.show, props.group?.id] as const, ([show]) => {
  if (show && props.group) void load()
  else loadVersion++
}, { immediate: true })
</script>
