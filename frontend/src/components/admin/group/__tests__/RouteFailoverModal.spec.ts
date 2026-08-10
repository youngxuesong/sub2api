import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import type { AdminGroup } from '@/types'
import RouteFailoverModal from '../RouteFailoverModal.vue'

const { getPolicy, savePolicy, deletePolicy, getGroups, showSuccess, showError } = vi.hoisted(() => ({
  getPolicy: vi.fn(),
  savePolicy: vi.fn(),
  deletePolicy: vi.fn(),
  getGroups: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: { groups: {
    getRouteFailoverPolicy: getPolicy,
    saveRouteFailoverPolicy: savePolicy,
    deleteRouteFailoverPolicy: deletePolicy,
    getAllIncludingInactive: getGroups
  } }
}))

vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess, showError }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const BaseDialogStub = defineComponent({
  props: { show: Boolean, title: String },
  emits: ['close'],
  template: '<section v-if="show"><h2>{{ title }}</h2><slot /></section>'
})

const sourceGroup = {
  id: 1,
  name: 'Primary',
  platform: 'anthropic',
  subscription_type: 'subscription',
  status: 'active'
} as AdminGroup

const candidates = [
  sourceGroup,
  { ...sourceGroup, id: 2, name: 'Matching target' },
  { ...sourceGroup, id: 3, name: 'Inactive target', status: 'inactive' },
  { ...sourceGroup, id: 4, name: 'Wrong platform', platform: 'openai' },
  { ...sourceGroup, id: 5, name: 'Wrong billing', subscription_type: 'standard' }
] as AdminGroup[]

function mountModal() {
  return mount(RouteFailoverModal, {
    props: { show: true, group: sourceGroup },
    global: { stubs: { BaseDialog: BaseDialogStub, Icon: true, PlatformIcon: true } }
  })
}

describe('RouteFailoverModal', () => {
  beforeEach(() => {
    getPolicy.mockReset()
    savePolicy.mockReset()
    deletePolicy.mockReset()
    getGroups.mockReset()
    showSuccess.mockReset()
    showError.mockReset()
    getGroups.mockResolvedValue(candidates)
    getPolicy.mockRejectedValue({ status: 404, code: 'ROUTE_FAILOVER_POLICY_NOT_FOUND' })
    savePolicy.mockImplementation(async (_id, payload) => ({ id: 8, source_group_id: 1, ...payload }))
  })

  it('uses defaults for a missing policy and only offers compatible active targets', async () => {
    const wrapper = mountModal()
    await flushPromises()

    expect(wrapper.get('[data-testid="failover-max-attempts"]').element).toHaveProperty('value', '3')
    const optionText = wrapper.get('[data-testid="failover-new-target"]').text()
    expect(optionText).toContain('Matching target')
    expect(optionText).not.toContain('Primary')
    expect(optionText).not.toContain('Inactive target')
    expect(optionText).not.toContain('Wrong platform')
    expect(optionText).not.toContain('Wrong billing')
    expect(showError).not.toHaveBeenCalled()
  })

  it('adds a target and model mapping, then sends the normalized payload', async () => {
    const wrapper = mountModal()
    await flushPromises()

    await wrapper.get('[data-testid="failover-new-target"]').setValue('2')
    await wrapper.get('[data-testid="failover-add-target"]').trigger('click')
    await wrapper.get('[data-testid="failover-add-mapping-0"]').trigger('click')
    await wrapper.get('[data-testid="failover-mapping-from-0-0"]').setValue(' claude-sonnet-4 ')
    await wrapper.get('[data-testid="failover-mapping-to-0-0"]').setValue(' claude-sonnet-4-5 ')
    await wrapper.get('[data-testid="failover-enabled"]').setValue(true)
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(savePolicy).toHaveBeenCalledWith(1, expect.objectContaining({
      enabled: true,
      max_attempts: 3,
      targets: [{
        target_group_id: 2,
        priority: 1,
        enabled: true,
        model_mapping: { 'claude-sonnet-4': 'claude-sonnet-4-5' }
      }]
    }))
    expect(showSuccess).toHaveBeenCalledWith('admin.groups.routeFailover.saved')
  })

  it('clears the previous group policy and hides save when the next load fails', async () => {
    getPolicy.mockResolvedValueOnce({
      id: 8,
      source_group_id: 1,
      enabled: true,
      max_attempts: 2,
      failure_threshold: 5,
      success_threshold: 2,
      window_seconds: 60,
      open_cooldown_seconds: 60,
      half_open_lease_seconds: 15,
      targets: [{
        id: 9,
        policy_id: 8,
        target_group_id: 2,
        priority: 1,
        enabled: true,
        model_mapping: { 'claude-source': 'claude-fallback' }
      }]
    }).mockRejectedValueOnce(new Error('policy unavailable'))
    getGroups.mockResolvedValueOnce(candidates).mockRejectedValueOnce(new Error('groups unavailable'))

    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.find('[data-testid="failover-mapping-from-0-0"]').exists()).toBe(true)

    await wrapper.setProps({ group: { ...sourceGroup, id: 6, name: 'Next group' } })
    await flushPromises()

    expect(wrapper.find('form').exists()).toBe(false)
    expect(wrapper.find('[data-testid="failover-load-error"]').exists()).toBe(true)
    expect(savePolicy).not.toHaveBeenCalled()
  })

  it('preserves an existing target id when saving an edited target', async () => {
    getPolicy.mockResolvedValue({
      id: 8,
      source_group_id: 1,
      enabled: true,
      max_attempts: 2,
      failure_threshold: 5,
      success_threshold: 2,
      window_seconds: 60,
      open_cooldown_seconds: 60,
      half_open_lease_seconds: 15,
      targets: [{
        id: 19,
        policy_id: 8,
        target_group_id: 2,
        priority: 1,
        enabled: true,
        model_mapping: {}
      }]
    })
    const wrapper = mountModal()
    await flushPromises()

    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(savePolicy).toHaveBeenCalledWith(1, expect.objectContaining({
      targets: [expect.objectContaining({ id: 19, target_group_id: 2 })]
    }))
  })
})
