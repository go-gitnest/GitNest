import { describe, expect, it } from 'vitest'
import { createTranslator } from './messages'

describe('the translator', () => {
  it('resolves an identifier from the catalogue the shell sent', () => {
    const t = createTranslator({ 'app.name': 'GitNest' })
    expect(t('app.name')).toBe('GitNest')
  })

  it('returns the identifier when the catalogue has no entry', () => {
    // The identifier stays findable in a test, where an empty string would look
    // like a layout defect.
    const t = createTranslator({})
    expect(t('app.missing')).toBe('app.missing')
  })

  it('substitutes placeholders in order', () => {
    const t = createTranslator({ 'app.signed_in_as': 'Signed in as %s' })
    expect(t('app.signed_in_as', 'alice')).toBe('Signed in as alice')
  })

  it('tolerates a missing argument rather than printing undefined', () => {
    const t = createTranslator({ 'app.signed_in_as': 'Signed in as %s' })
    expect(t('app.signed_in_as')).toBe('Signed in as ')
  })
})