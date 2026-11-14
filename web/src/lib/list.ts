// Keeps the message list in step with what the reading pane shows. htmx only
// swaps the pane; without this the clicked row would keep looking unread and
// unselected until the list is next reloaded.
export function initListState(): void {
  // Capture phase: htmx handles a row click by calling preventDefault(), which
  // a bubbling listener on document would see as "already handled".
  document.addEventListener('click', (e) => {
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
    const row = (e.target as Element).closest<HTMLElement>('.message-row')
    if (!row) return
    document.querySelectorAll('.message-row.selected, .message-row.kb-focus').forEach((r) => {
      r.classList.remove('selected', 'kb-focus')
    })
    row.classList.add('selected')
    if (row.classList.contains('unread')) {
      row.classList.remove('unread') // opening a message marks it \Seen server-side
      dropUnreadCount()
    }
  }, true)
}

// Keep the sidebar's unread badge for the current folder in step.
function dropUnreadCount(): void {
  const badge = document.querySelector<HTMLElement>('.folder-link.active .count')
  if (!badge) return
  const next = (parseInt(badge.textContent ?? '', 10) || 0) - 1
  if (next > 0) {
    badge.textContent = String(next)
    badge.setAttribute('aria-label', `${next} unread`)
  } else {
    badge.remove()
  }
}
