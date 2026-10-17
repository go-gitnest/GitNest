// Fails on a user-visible string literal in a component.
//
// `frontend.md` states the rule and asks for exactly this check: "No
// user-visible string is hardcoded in a Go template or a React component.
// Strings are referenced by identifier from a message catalogue, and CI fails on
// a new hardcoded literal in a user-visible position."
//
// The check is deliberately narrow, because a broad one would be ignored. It
// reads JSX text nodes and the attributes that reach a reader, and it allows
// anything that is punctuation, a number, or a catalogued identifier.
import { readFile, readdir } from 'node:fs/promises'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const source = join(here, '..', 'src')

/** Attributes whose value a reader sees or hears. */
const VISIBLE_ATTRIBUTES = ['label', 'title', 'alt', 'placeholder', 'aria-label', 'aria-description', 'legend']

/** A literal that is only punctuation, whitespace, or a number needs no message. */
function isTranslatable(text) {
  const trimmed = text.trim()
  if (trimmed === '') return false
  if (/^[\p{P}\p{S}\s]+$/u.test(trimmed)) return false
  if (/^[\d\s.,:%+-]+$/.test(trimmed)) return false
  // A single character is a symbol rather than a sentence.
  if ([...trimmed].length === 1) return false
  return /\p{L}/u.test(trimmed)
}

/** Lists every source file to check. */
async function listSource(root) {
  const found = []
  async function walk(current) {
    const entries = await readdir(current, { withFileTypes: true })
    for (const entry of entries) {
      const full = join(current, entry.name)
      if (entry.isDirectory()) await walk(full)
      else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name)) found.push(full)
    }
  }
  await walk(root)
  return found
}

const files = await listSource(source)
const violations = []

for (const file of files) {
  const text = await readFile(file, 'utf8')
  const lines = text.split('\n')

  lines.forEach((line, index) => {
    const where = `${relative(here, file)}:${index + 1}`

    // A JSX text node: a run of words between a tag's closing bracket and the
    // next tag.
    //
    // The pattern requires the opening `>` to end a tag, which is what separates
    // this from a TypeScript type argument or a comparison. An earlier version
    // matched those and reported `useState<...>` and `as Partial<...>` as
    // user-visible text; a check that cries wolf is a check that gets ignored, so
    // the pattern is narrow on purpose.
    for (const match of line.matchAll(/<\/?[A-Za-z][^<>]*>\s*([A-Za-z][^<>{}]*?)\s*</g)) {
      const literal = match[1]
      if (literal && isTranslatable(literal)) {
        violations.push(`${where}: JSX text ${JSON.stringify(literal.trim())}`)
      }
    }

    // A visible attribute with a literal string value.
    for (const attribute of VISIBLE_ATTRIBUTES) {
      const pattern = new RegExp(`${attribute}=(["'])(.*?)\\1`, 'g')
      for (const match of line.matchAll(pattern)) {
        const literal = match[2]
        if (literal && isTranslatable(literal)) {
          violations.push(`${where}: ${attribute} ${JSON.stringify(literal)}`)
        }
      }
    }
  })
}

if (violations.length > 0) {
  console.error('user-visible strings must come from the message catalogue:')
  for (const violation of violations) console.error(`  ${violation}`)
  process.exit(1)
}

console.log(`no hardcoded user-visible strings in ${files.length} source files`)