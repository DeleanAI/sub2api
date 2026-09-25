import { describe, expect, it } from 'vitest'

import {
  CONFIGURABLE_OPENAI_ENDPOINT_CAPABILITIES,
  DEFAULT_OPENAI_ENDPOINT_CAPABILITIES,
  isDefaultOpenAIEndpointCapabilitySet,
  isOpenAIEndpointCapability,
  normalizeOpenAIEndpointCapabilities
} from '../openaiEndpointCapabilities'

describe('openaiEndpointCapabilities', () => {
  it('keeps declared order and falls back to the default set', () => {
    expect(normalizeOpenAIEndpointCapabilities(['seedance', 'chat_completions'])).toEqual([
      'chat_completions',
      'seedance'
    ])
    expect(normalizeOpenAIEndpointCapabilities([])).toEqual([...DEFAULT_OPENAI_ENDPOINT_CAPABILITIES])
  })

  it('treats only the default set as "not configured"', () => {
    expect(isDefaultOpenAIEndpointCapabilitySet(['embeddings', 'chat_completions'])).toBe(true)
    expect(isDefaultOpenAIEndpointCapabilitySet(['chat_completions', 'seedance'])).toBe(false)
    expect(isDefaultOpenAIEndpointCapabilitySet(['seedance'])).toBe(false)
  })

  it('recognises every configurable capability and nothing else', () => {
    for (const capability of CONFIGURABLE_OPENAI_ENDPOINT_CAPABILITIES) {
      expect(isOpenAIEndpointCapability(capability)).toBe(true)
    }
    expect(isOpenAIEndpointCapability('responses')).toBe(false)
    expect(isOpenAIEndpointCapability(1)).toBe(false)
  })
})
