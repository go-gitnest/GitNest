// Removes the previous build's output before a new one, keeping the placeholder
// the repository commits.
//
// It is a script rather than a dependency because it is a directory listing and
// an unlink, which the standard library does; adding a package for that would be
// the dependency the frontend rules rule out.
import { readdir, rm } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const dist = join(here, '..', 'dist')

let entries
try {
  entries = await readdir(dist)
} catch (error) {
  // Nothing to clean on a checkout that has never been built.
  if (error.code === 'ENOENT') process.exit(0)
  throw error
}

for (const entry of entries) {
  if (entry === '.gitkeep') continue
  await rm(join(dist, entry), { recursive: true, force: true })
}
