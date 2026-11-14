import htmx from 'htmx.org'

import './styles/tokens.css'
import './styles/base.css'
import './styles/mail.css'
import './styles/compose.css'

import { fitHtmlFrames } from './lib/frames'
import { mountIslands } from './lib/islands'
import { initListState } from './lib/list'
import { initShortcuts } from './lib/shortcuts'

declare global {
  interface Window {
    htmx: typeof htmx
  }
}
// Nothing here needs eval or <script> tags in swapped content; turn both off.
htmx.config.allowEval = false
htmx.config.allowScriptTags = false
window.htmx = htmx

function enhance(root: ParentNode = document): void {
  fitHtmlFrames(root)
  mountIslands(root)
}

initShortcuts()
initListState()
enhance()
// htmx swaps in new fragments (message view, list); enhance those too.
document.addEventListener('htmx:load', (e) => enhance((e.target as ParentNode) ?? document))
