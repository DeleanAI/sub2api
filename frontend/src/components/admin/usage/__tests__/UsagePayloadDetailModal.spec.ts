import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent } from 'vue'

import UsagePayloadDetailModal from '../UsagePayloadDetailModal.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /></div>',
})

const buildPayload = (request: unknown, response: unknown) => ({
  request_body: JSON.stringify(request),
  response_body: JSON.stringify(response),
  metadata: {},
  stored_bytes: 512,
  created_at: '2026-08-27T00:00:00Z',
  updated_at: '2026-08-27T00:00:00Z',
})

const mountModal = (request: unknown, response: unknown) => mount(UsagePayloadDetailModal, {
  props: {
    show: true,
    payload: buildPayload(request, response),
    loading: false,
    unavailable: false,
  },
  global: {
    stubs: { BaseDialog: BaseDialogStub },
  },
})

describe('UsagePayloadDetailModal', () => {
  it('renders the final user message and model response as sanitized Markdown', () => {
    const wrapper = mountModal(
      {
        messages: [
          { role: 'user', content: 'Earlier question' },
          { role: 'assistant', content: 'Earlier answer' },
          { role: 'user', content: '## Latest question\n\n- one\n- two' },
        ],
      },
      {
        choices: [{
          message: {
            role: 'assistant',
            content: '## Latest answer\n\n**Done**<script>window.__payloadXss = true</script>',
          },
        }],
      },
    )

    const userPreview = wrapper.get('[data-testid="last-user-markdown"]')
    const modelPreview = wrapper.get('[data-testid="model-output-markdown"]')

    expect(userPreview.text()).toContain('Latest question')
    expect(userPreview.text()).not.toContain('Earlier question')
    expect(userPreview.find('h2').text()).toBe('Latest question')
    expect(modelPreview.find('h2').text()).toBe('Latest answer')
    expect(modelPreview.find('strong').text()).toBe('Done')
    expect(modelPreview.find('script').exists()).toBe(false)
  })

  it('supports OpenAI Responses input and output text blocks', () => {
    const wrapper = mountModal(
      {
        input: [
          { role: 'user', content: [{ type: 'input_text', text: 'First turn' }] },
          { role: 'user', content: [{ type: 'input_text', text: 'Last turn' }] },
        ],
      },
      {
        output: [{
          role: 'assistant',
          content: [{ type: 'output_text', text: 'Final response' }],
        }],
      },
    )

    expect(wrapper.get('[data-testid="last-user-markdown"]').text()).toContain('Last turn')
    expect(wrapper.get('[data-testid="model-output-markdown"]').text()).toContain('Final response')
  })
})
