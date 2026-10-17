import { describe, expect, it } from 'vitest'
import { readBoot } from './boot'

function documentWith(payload: string): Document {
  const doc = document.implementation.createHTMLDocument('test')
  const element = doc.createElement('script')
  element.type = 'application/json'
  element.id = 'gitnest-boot'
  element.textContent = payload
  doc.body.append(element)
  return doc
}

describe('reading the boot payload', () => {
  it('reads the locale, area, account, and messages', () => {
    const doc = documentWith(
      JSON.stringify({ locale: 'en-US', area: 'create', account: 'alice', csrf_token: 't', messages: { a: 'b' } }),
    )
    const payload = readBoot(doc)
    expect(payload.locale).toBe('en-US')
    expect(payload.area).toBe('create')
    expect(payload.account).toBe('alice')
    expect(payload.csrf_token).toBe('t')
    expect(payload.messages).toEqual({ a: 'b' })
  })

  it('omits the session fields when the caller is anonymous', () => {
    const doc = documentWith(JSON.stringify({ locale: 'en-US', area: 'create', messages: {} }))
    const payload = readBoot(doc)
    expect(payload.account).toBeUndefined()
    expect(payload.csrf_token).toBeUndefined()
  })

  it('throws when the element is missing', () => {
    // Throwing rather than defaulting: a client that guessed its own messages or
    // session would render something the server did not intend.
    const doc = document.implementation.createHTMLDocument('test')
    expect(() => readBoot(doc)).toThrow(/missing/)
  })

  it('throws on a malformed payload', () => {
    expect(() => readBoot(documentWith('not json'))).toThrow()
  })
})