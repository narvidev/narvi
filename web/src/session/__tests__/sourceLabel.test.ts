// sourceLabel.test.ts -- Session.spawnSource is an OPEN enum
// (contracts/manifest.json's openEnums): the words for a known source
// stay exactly what they were, and a source this bundle has never heard
// of gets neutral words, never another source's and never an empty tag.
import { describe, expect, it } from 'vitest'

import { isKnownSpawnSource, sourceTagLabel, sourceTitle } from '../sourceLabel'

describe('sourceLabel', () => {
  it.each([
    ['web', 'Started from the web app', 'web'],
    ['slack', 'Started from Slack', 'Slack'],
    ['linear', 'Started from Linear', 'Linear'],
    ['github', 'Started from GitHub', 'GitHub'],
    ['mcp', 'Started from an MCP client', 'MCP'],
  ])('names the known source %s', (source, title, tag) => {
    expect(isKnownSpawnSource(source)).toBe(true)
    expect(sourceTitle(source)).toBe(title)
    expect(sourceTagLabel(source)).toBe(tag)
  })

  it.each(['some_future_source', '', 'Slack', 'MCP', 'constructor', 'toString', '__proto__'])(
    'gives the unknown source %j neutral words, never a known source\'s',
    (source) => {
      expect(isKnownSpawnSource(source)).toBe(false)
      expect(sourceTitle(source)).toBe('Started from another source')
      expect(sourceTagLabel(source)).toBe('other')
    },
  )
})
