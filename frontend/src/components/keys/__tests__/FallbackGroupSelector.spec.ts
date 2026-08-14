import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import FallbackGroupSelector from '@/components/keys/FallbackGroupSelector.vue'
import type { Group } from '@/types'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => params
      ? `${key}:${JSON.stringify(params)}`
      : key
  })
}))

const SelectStub = defineComponent({
  props: {
    modelValue: { type: [String, Number, Boolean], default: null },
    options: { type: Array, default: () => [] }
  },
  emits: ['update:modelValue'],
  template: `
    <select
      data-testid="fallback-group-select"
      :value="modelValue ?? ''"
      @change="$emit('update:modelValue', Number($event.target.value))"
    >
      <option value="">Choose</option>
      <option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option>
    </select>
  `
})

const groups = [
  { id: 1, name: 'Primary', platform: 'openai' },
  { id: 2, name: 'Fallback 2', platform: 'openai' },
  { id: 3, name: 'Fallback 3', platform: 'openai' },
  { id: 4, name: 'Fallback 4', platform: 'openai' },
  { id: 5, name: 'Fallback 5', platform: 'openai' },
  { id: 6, name: 'Fallback 6', platform: 'openai' },
  { id: 7, name: 'Fallback 7', platform: 'openai' }
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
