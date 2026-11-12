import { describe, expect, it } from 'vitest'

import { safeSessionURL } from './sessionLink'

describe('safeSessionURL', () => {
  it.each([
    ['https://agentsview.example/sessions/a', 'https://agentsview.example/sessions/a'],
    ['http://127.0.0.1:8080/sessions/a', 'http://127.0.0.1:8080/sessions/a'],
    ['javascript:alert(1)', undefined],
    ['https://example-user@agentsview.example/sessions/a', undefined],
    ['https://agentsview.example/sessions/a?token=x', undefined],
    ['https://agentsview.example/sessions/a#part', undefined],
    ['/sessions/a', undefined],
    [42, undefined],
  ])('accepts %s as %s', (value, url) => {
    expect(safeSessionURL(value)).toBe(url)
  })
})
