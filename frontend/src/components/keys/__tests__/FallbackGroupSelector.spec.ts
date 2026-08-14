import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import FallbackGroupSelector from '@/components/keys/FallbackGroupSelector.vue'
import type { Group } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => params
        ? `${key}:${JSON.stringify(params)}`
        : key
    })
  }
})

const SelectStub = defineComponent({
  inheritAttrs: false,
  props: {
    modelValue: { type: [String, Number, Boolean], default: null },
    options: { type: Array, default: () => [] }
  },
  emits: ['update:modelValue'],
  template: `
    <div>
      <select
        data-testid="fallback-group-select"
        :value="modelValue ?? ''"
        @change="$emit('update:modelValue', Number($event.target.value))"
      >
        <option value="">Choose</option>
        <option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option>
      </select>
      <div data-testid="rendered-fallback-options">
        <div v-for="option in options" :key="option.value">
          <slot name="option" :option="option" :selected="false">{{ option.label }}</slot>
        </div>
      </div>
    </div>
  `
})

const GroupOptionItemStub = defineComponent({
  props: {
    name: { type: String, required: true },
    rateMultiplier: { type: Number, default: undefined },
    userRateMultiplier: { type: Number, default: null }
  },
  template: `
    <span data-testid="fallback-group-option">
      {{ name }}|{{ rateMultiplier }}x|{{ userRateMultiplier ?? 'default' }}
    </span>
  `
})

const groups = [
  { id: 1, name: 'Primary', platform: 'openai', rate_multiplier: 1 },
  { id: 2, name: 'Fallback 2', platform: 'openai', rate_multiplier: 2 },
  { id: 3, name: 'Fallback 3', platform: 'openai', rate_multiplier: 3 },
  { id: 4, name: 'Fallback 4', platform: 'openai', rate_multiplier: 4 },
  { id: 5, name: 'Fallback 5', platform: 'openai', rate_multiplier: 5 },
  { id: 6, name: 'Fallback 6', platform: 'openai', rate_multiplier: 6 },
  { id: 7, name: 'Fallback 7', platform: 'openai', rate_multiplier: 7 }
] as unknown as Group[]

const mountSelector = (props: Record<string, unknown> = {}) => mount(FallbackGroupSelector, {
  props: {
    groups,
    primaryGroupId: 1,
    modelValue: [],
    riskAcknowledged: false,
    ...props
  },
  global: {
    stubs: {
      Select: SelectStub,
      GroupOptionItem: GroupOptionItemStub,
      Icon: true
    }
  }
})

describe('FallbackGroupSelector', () => {
  it('excludes the primary and already selected groups from the add list', async () => {
    const wrapper = mountSelector({ modelValue: [2] })

    const options = wrapper.find('[data-testid="fallback-group-select"]').findAll('option')
    expect(options.map((option) => option.text())).toEqual([
      'Choose',
      'Fallback 3',
      'Fallback 4',
      'Fallback 5',
      'Fallback 6',
      'Fallback 7'
    ])
  })

  it('shows each fallback group rate and the user-specific rate override', () => {
    const wrapper = mountSelector({ userGroupRates: { 2: 1.25 } })

    const options = wrapper.findAll('[data-testid="fallback-group-option"]')
    expect(options.map((option) => option.text())).toContain('Fallback 2|2x|1.25')
    expect(options.map((option) => option.text())).toContain('Fallback 3|3x|default')
  })

  it('emits a selected group and resets risk acknowledgement', async () => {
    const wrapper = mountSelector({ modelValue: [2], riskAcknowledged: true })

    await wrapper.find('[data-testid="fallback-group-select"]').setValue('3')

    expect(wrapper.emitted('update:modelValue')).toEqual([[[2, 3]]])
    expect(wrapper.emitted('update:riskAcknowledged')).toEqual([[false]])
  })

  it('supports ordering and requires an explicit risk acknowledgement', async () => {
    const wrapper = mountSelector({ modelValue: [2, 3] })

    await wrapper.get('[data-testid="fallback-move-down-2"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')).toEqual([[[3, 2]]])

    await wrapper.get('[data-testid="fallback-risk-checkbox"]').setValue(true)
    expect(wrapper.emitted('update:riskAcknowledged')).toEqual([[false], [true]])
  })
})
