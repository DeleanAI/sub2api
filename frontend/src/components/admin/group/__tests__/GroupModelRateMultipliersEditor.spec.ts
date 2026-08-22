import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import GroupModelRateMultipliersEditor from '../GroupModelRateMultipliersEditor.vue'
import type { ModelRateMultiplierFormEntry } from '@/views/admin/groupsModelRateMultipliers'

const messages: Record<string, string> = {
  'admin.groups.modelRateMultipliers.title': 'Per-model rate multipliers',
  'admin.groups.modelRateMultipliers.description': 'desc',
  'admin.groups.modelRateMultipliers.add': 'Add rule',
  'admin.groups.modelRateMultipliers.empty': 'No rules',
  'admin.groups.modelRateMultipliers.pattern': 'Model pattern',
  'admin.groups.modelRateMultipliers.patternPlaceholder': 'e.g. claude-opus-*',
  'admin.groups.modelRateMultipliers.multiplier': 'Factor',
  'admin.groups.modelRateMultipliers.preview': 'Preview',
  'admin.groups.modelRateMultipliers.previewFormula': '{base} × {factor} = {effective}',
  'admin.groups.modelRateMultipliers.orderHint': 'order hint',
  'admin.groups.modelRateMultipliers.patternRequired': 'Model pattern is required',
  'admin.groups.modelRateMultipliers.multiplierRange': 'Factor must be greater than 0 and at most 100',
  'admin.groups.modelRateMultipliers.duplicatePattern': 'Duplicate model pattern',
  'common.delete': 'Delete',
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => {
        let text = messages[key] ?? key
        for (const [name, value] of Object.entries(params ?? {})) {
          text = text.replace(`{${name}}`, String(value))
        }
        return text
      },
    }),
  }
})

const mountEditor = (entries: ModelRateMultiplierFormEntry[], baseRateMultiplier: number | string | null = 1.5) =>
  mount(GroupModelRateMultipliersEditor, {
    props: { entries, baseRateMultiplier },
    global: { stubs: { Icon: true } },
  })

describe('GroupModelRateMultipliersEditor', () => {
  it('shows the empty hint and emits a new blank row on add', async () => {
    const wrapper = mountEditor([])
    expect(wrapper.text()).toContain('No rules')

    await wrapper.get('[data-testid="model-rate-multipliers-add"]').trigger('click')

    expect(wrapper.emitted('update:entries')).toEqual([[[{ model_pattern: '', multiplier: null }]]])
  })

  it('previews the effective rate as base × factor and emits edits/removals', async () => {
    const wrapper = mountEditor([{ model_pattern: 'claude-opus-*', multiplier: 2 }], 1.5)
    expect(wrapper.text()).toContain('1.5 × 2 = 3')
    expect(wrapper.find('[data-testid="model-rate-multiplier-error"]').exists()).toBe(false)

    await wrapper.get('[data-testid="model-rate-multiplier-value"]').setValue('0.5')
    expect(wrapper.emitted('update:entries')?.[0]).toEqual([[{ model_pattern: 'claude-opus-*', multiplier: '0.5' }]])

    await wrapper.get('[data-testid="model-rate-multiplier-remove"]').trigger('click')
    expect(wrapper.emitted('update:entries')?.[1]).toEqual([[]])
  })

  it('flags the first invalid row with the matching validation message', () => {
    const wrapper = mountEditor([
      { model_pattern: 'claude-opus-*', multiplier: 2 },
      { model_pattern: 'Claude-Opus-*', multiplier: 3 },
      { model_pattern: '', multiplier: 1 },
    ])
    const errors = wrapper.findAll('[data-testid="model-rate-multiplier-error"]')
    expect(errors).toHaveLength(1)
    expect(errors[0].text()).toBe('Duplicate model pattern')
  })

  it('flags an out-of-range factor', () => {
    const wrapper = mountEditor([{ model_pattern: 'gpt-5*', multiplier: 101 }])
    expect(wrapper.get('[data-testid="model-rate-multiplier-error"]').text()).toContain('at most 100')
  })
})
