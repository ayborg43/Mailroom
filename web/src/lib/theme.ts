// Theme switch: System (follow the OS), Light or Dark. An explicit choice is
// stored in localStorage and applied as <html data-theme="…">; "system"
// removes the attribute so the CSS media query decides. The same attribute is
// also set by a tiny inline script in each page's <head> (templates/head.html)
// before first paint, so there's no flash of the wrong theme.
type Choice = 'system' | 'light' | 'dark'

const KEY = 'mail:theme'

function stored(): Choice {
  try {
    const v = localStorage.getItem(KEY)
    return v === 'light' || v === 'dark' ? v : 'system'
  } catch {
    return 'system'
  }
}

function render(choice: Choice): void {
  const root = document.documentElement
  if (choice === 'system') delete root.dataset.theme
  else root.dataset.theme = choice
  document.querySelectorAll<HTMLElement>('[data-theme-choice]').forEach((b) => {
    b.setAttribute('aria-pressed', String(b.dataset.themeChoice === choice))
  })
}

function choose(choice: Choice): void {
  try {
    if (choice === 'system') localStorage.removeItem(KEY)
    else localStorage.setItem(KEY, choice)
  } catch {
    // Storage blocked: the choice still applies until the page is closed.
  }
  render(choice)
}

export function initTheme(): void {
  render(stored())
  document.addEventListener('click', (e) => {
    const btn = (e.target as Element).closest<HTMLElement>('[data-theme-choice]')
    if (btn) choose(btn.dataset.themeChoice as Choice)
  })
  // Another tab changed the theme.
  window.addEventListener('storage', (e) => {
    if (e.key === KEY) render(stored())
  })
}
