// Gmail-style keyboard shortcuts for the mail view. They never fire while
// typing in a field or with a modifier held, so they don't fight the browser.
const HELP: [string, string][] = [
  ['j / k', 'Next / previous message'],
  ['Enter or o', 'Open the highlighted message'],
  ['u', 'Back to the list'],
  ['c', 'Compose'],
  ['r', 'Reply'],
  ['a', 'Reply all'],
  ['f', 'Forward'],
  ['s', 'Flag / unflag'],
  ['#', 'Delete'],
  ['/', 'Search'],
  ['?', 'This help'],
]

const FOCUS_CLASS = 'kb-focus'

function isTyping(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null
  if (!el) return false
  return el.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName)
}

function rows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('.message-list .message-row'))
}

function moveFocus(delta: 1 | -1): void {
  const list = rows()
  if (list.length === 0) return
  let i = list.findIndex((r) => r.classList.contains(FOCUS_CLASS))
  if (i === -1) i = list.findIndex((r) => r.classList.contains('selected'))
  const next = i === -1 ? (delta === 1 ? 0 : list.length - 1) : Math.min(Math.max(i + delta, 0), list.length - 1)
  list.forEach((r) => r.classList.remove(FOCUS_CLASS))
  list[next].classList.add(FOCUS_CLASS)
  list[next].scrollIntoView({ block: 'nearest' })
}

function click(selector: string): boolean {
  const el = document.querySelector<HTMLElement>(selector)
  el?.click()
  return el !== null
}

let helpDialog: HTMLDialogElement | null = null

function showHelp(): void {
  if (!helpDialog) {
    const dlg = document.createElement('dialog')
    dlg.className = 'shortcut-help'
    const h = document.createElement('h2')
    h.textContent = 'Keyboard shortcuts'
    const dl = document.createElement('dl')
    for (const [keys, what] of HELP) {
      const dt = document.createElement('dt')
      keys.split(' ').forEach((part) => {
        if (part === '/' || part === 'or') dt.append(` ${part} `)
        else {
          const k = document.createElement('kbd')
          k.textContent = part
          dt.append(k)
        }
      })
      const dd = document.createElement('dd')
      dd.textContent = what
      dl.append(dt, dd)
    }
    const close = document.createElement('button')
    close.className = 'btn'
    close.textContent = 'Close'
    close.addEventListener('click', () => dlg.close())
    dlg.append(h, dl, close)
    dlg.addEventListener('click', (e) => {
      if (e.target === dlg) dlg.close() // click on the backdrop
    })
    document.body.append(dlg)
    helpDialog = dlg
  }
  if (!helpDialog.open) helpDialog.showModal()
}

function onKeyDown(e: KeyboardEvent): void {
  if (e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented) return
  if (isTyping(e.target) || helpDialog?.open) return

  const inMail = document.querySelector('.message-list') !== null
  let handled = true
  switch (e.key) {
    case '?':
      showHelp()
      break
    case 'c':
      location.assign('/compose')
      break
    case '/': {
      const search = document.querySelector<HTMLInputElement>('.list-toolbar input[type="search"]')
      if (search) search.focus()
      else handled = false
      break
    }
    case 'j':
      if (inMail) moveFocus(1)
      else handled = false
      break
    case 'k':
      if (inMail) moveFocus(-1)
      else handled = false
      break
    case 'Enter':
    case 'o': {
      const row = document.querySelector<HTMLElement>(`.message-row.${FOCUS_CLASS}`)
      if (row) row.click()
      else handled = false
      break
    }
    case 'u':
      handled = click('.back-link') || click('.folder-link.active')
      break
    case 'r':
      handled = click('.message-actions a[href*="mode=reply&"]')
      break
    case 'a':
      handled = click('.message-actions a[href*="mode=replyall&"]')
      break
    case 'f':
      handled = click('.message-actions a[href*="mode=forward&"]')
      break
    case 's':
      handled = click('.message-actions [data-action="flag"]')
      break
    case '#':
      // Moves to Trash (the form itself asks first when deleting from Trash).
      handled = click('.message-actions [data-action="delete"]')
      break
    default:
      handled = false
  }
  if (handled) e.preventDefault()
}

export function initShortcuts(): void {
  document.addEventListener('keydown', onKeyDown)
  document.addEventListener('click', (e) => {
    if ((e.target as HTMLElement).closest('[data-action="shortcuts-help"]')) showHelp()
  })
}
