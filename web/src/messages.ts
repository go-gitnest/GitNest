// Resolves message identifiers from the catalogue the shell sent.
//
// `frontend.md` requires that no user-visible string is hardcoded and that both
// sides read one identifier namespace. The server sends the entries this surface
// needs, so a message is translated once and a component never carries English.
//
// A missing identifier returns the identifier itself, matching the server's
// behaviour: it is findable in a test, whereas an empty string looks like a
// layout defect.

export type Messages = Record<string, string>

/** A message lookup bound to one payload's catalogue entries. */
export type Translate = (id: string, ...args: unknown[]) => string

/**
 * createTranslator returns a lookup over the payload's messages.
 *
 * Substitution uses `%s` placeholders, as the server does, rather than
 * concatenation: a sentence assembled from fragments cannot be reordered by a
 * translator.
 */
export function createTranslator(messages: Messages): Translate {
  return (id, ...args) => {
    const message = messages[id] ?? id
    if (args.length === 0) return message
    let index = 0
    return message.replace(/%s/g, () => {
      const value = args[index]
      index += 1
      return value === undefined ? '' : String(value)
    })
  }
}