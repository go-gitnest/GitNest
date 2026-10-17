import './styles.css'

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './app'
import { readBoot } from './boot'

// The shell guarantees the element exists; a missing one is a wiring fault, and
// failing loudly is better than rendering nothing.
const container = document.getElementById('root')
if (!container) {
  throw new Error('the mount element #root is missing')
}

createRoot(container).render(
  <StrictMode>
    <App boot={readBoot()} />
  </StrictMode>,
)