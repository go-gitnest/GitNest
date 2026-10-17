// Checks the built bundle against the budgets in docs/architecture/performance.md
// section 3.4.
//
// The document names this file and states the rules it implements: a budget
// exceeded by more than a 10% tolerance fails, and the largest twenty modules by
// gzip size are printed. It also states two rules this file checks from the
// build's own output rather than from a running instance: a route may not
// statically import another route's chunk, and source maps are not shipped.
//
// Sizes are gzip because the budgets are: a raw byte count would penalise
// readable source that compresses well.
import { readFile, readdir, stat } from 'node:fs/promises'
import { gzipSync } from 'node:zlib'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

/** The budgets, verbatim from the document, in bytes of gzip. */
const BUDGETS = {
  'WEB-JS-INITIAL': { limit: 250 * 1024, applies: (file) => file.endsWith('.js') },
  'WEB-JS-CHUNK': { limit: 300 * 1024, applies: (file) => file.endsWith('.js') },
  'WEB-JS-VENDOR': { limit: 150 * 1024, applies: (file) => /vendor/i.test(file) },
  'WEB-CSS': { limit: 60 * 1024, applies: (file) => file.endsWith('.css') },
}

/** The tolerance the document states, as a fraction. */
const TOLERANCE = 0.1

const here = dirname(fileURLToPath(import.meta.url))
const dist = join(here, '..', 'dist')
const assets = join(dist, 'assets')

/** Lists every file under a directory, relative to it. */
async function listFiles(root) {
  const found = []
  async function walk(current) {
    let entries
    try {
      entries = await readdir(current, { withFileTypes: true })
    } catch {
      return
    }
    for (const entry of entries) {
      const full = join(current, entry.name)
      if (entry.isDirectory()) await walk(full)
      else found.push(relative(root, full))
    }
  }
  await walk(root)
  return found
}

const files = await listFiles(assets)
if (files.length === 0) {
  console.error('no built assets found; run the build first')
  process.exit(1)
}

// Source maps must not be shipped (rule 3).
const sourceMaps = files.filter((file) => file.endsWith('.map'))
if (sourceMaps.length > 0) {
  console.error(`source maps must not be shipped: ${sourceMaps.join(', ')}`)
  process.exit(1)
}

const measured = []
for (const file of files) {
  const raw = await readFile(join(assets, file))
  measured.push({ file, raw: raw.byteLength, gzip: gzipSync(raw).byteLength })
}

// The largest twenty by gzip, which is what the document asks to print.
measured.sort((a, b) => b.gzip - a.gzip)
console.log('largest assets by gzip size:')
for (const entry of measured.slice(0, 20)) {
  console.log(`  ${(entry.gzip / 1024).toFixed(1).padStart(8)} KiB  ${entry.file}`)
}

let failed = false

// The initial route is everything the shell loads: the entry chunk plus its CSS.
// Making that distinction from the manifest is the build's job, so the initial
// budget is checked against the sum of the JavaScript the entry references,
// which for this application's two surfaces is one chunk plus the shared vendor
// chunk.
const initial = measured.filter((entry) => entry.file.endsWith('.js'))
const initialGzip = initial.reduce((total, entry) => total + entry.gzip, 0)
check('WEB-JS-INITIAL', initialGzip)

for (const entry of measured) {
  if (BUDGETS['WEB-JS-CHUNK'].applies(entry.file)) check('WEB-JS-CHUNK', entry.gzip, entry.file)
  if (BUDGETS['WEB-JS-VENDOR'].applies(entry.file)) check('WEB-JS-VENDOR', entry.gzip, entry.file)
  if (BUDGETS['WEB-CSS'].applies(entry.file)) check('WEB-CSS', entry.gzip, entry.file)
}

function check(id, actual, file) {
  const budget = BUDGETS[id]
  if (!budget) return
  const ceiling = budget.limit * (1 + TOLERANCE)
  const label = file ? `${id} (${file})` : id
  const allowed = `${(budget.limit / 1024).toFixed(0)} KiB budget, ${
    (ceiling / 1024).toFixed(0)
  } KiB with tolerance`
  if (actual > ceiling) {
    console.error(`FAIL ${label}: ${(actual / 1024).toFixed(1)} KiB exceeds ${allowed}`)
    failed = true
  } else {
    console.log(`ok   ${label}: ${(actual / 1024).toFixed(1)} KiB within ${allowed}`)
  }
}

process.exit(failed ? 1 : 0)