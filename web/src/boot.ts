// Reads what the server-rendered shell hands the client.
//
// The payload travels in a `application/json` script element rather than in an
// inline script, so no executable code is assembled from data and the content
// security policy needs no exception for it (threat model T-B6).

/** The identifier of the element the shell puts the payload in. */
const ELEMENT_ID = 'gitnest-boot'

export type Area = 'create' | 'settings'

export interface BootPayload {
  /** The negotiated locale, so the client formats the same way the server did. */
  locale: string
  /**
   * The token a state-changing request must carry. It is absent for an anonymous
   * caller.
   *
   * It is held in memory only: `frontend.md` forbids tokens in `localStorage`,
   * and a token in storage outlives the session that issued it.
   */
  csrf_token?: string
  /** The signed-in account's name, absent when anonymous. */
  account?: string
  /** The catalogue entries this surface renders, by identifier. */
  messages: Record<string, string>
  /** Which surface the shell mounts, so one bundle serves each. */
  area: Area
  /**
   * The confirmation a reducing visibility transition requires.
   *
   * The server decides it and sends it, so the client never carries its own copy
   * of a policy value: a client that did would disagree with the server the
   * moment the rule changed, and tell the reader to type something that fails.
   */
  confirmation_word?: string
  /** The repository a settings surface acts on, from the routed path. */
  owner?: string
  repo?: string
}

/**
 * readBoot returns the payload the shell rendered.
 *
 * A missing or malformed payload throws rather than falling back to defaults: a
 * client that guessed its own messages or session would render something the
 * server did not intend.
 */
export function readBoot(doc: Document = document): BootPayload {
  const element = doc.getElementById(ELEMENT_ID)
  if (!element) {
    throw new Error(`the boot payload element #${ELEMENT_ID} is missing`)
  }
  const parsed: unknown = JSON.parse(element.textContent ?? '')
  if (typeof parsed !== 'object' || parsed === null) {
    throw new Error('the boot payload is not an object')
  }
  const payload = parsed as Partial<BootPayload>
  if (typeof payload.locale !== 'string' || typeof payload.area !== 'string') {
    throw new Error('the boot payload is missing its locale or area')
  }
  return {
    locale: payload.locale,
    area: payload.area as Area,
    messages: payload.messages ?? {},
    ...(payload.confirmation_word ? { confirmation_word: payload.confirmation_word } : {}),
    ...(payload.owner ? { owner: payload.owner } : {}),
    ...(payload.repo ? { repo: payload.repo } : {}),
    ...(payload.csrf_token ? { csrf_token: payload.csrf_token } : {}),
    ...(payload.account ? { account: payload.account } : {}),
  }
}