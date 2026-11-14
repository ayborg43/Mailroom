// Keeps the message list in step with what the reading pane shows. htmx only
// swaps the pane; without this the clicked row would keep looking unread and
// unselected until the list is next reloaded.
export function initListState(): void {
  document.addEventListener('click', (e) => {
    if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
    const row = (e.target as Element).closest<HTMLElement>('.message-row')
    if (!row) return
    document.querySelectorAll('.message-row.selected, .message-row.kb-focus').forEach((r) => {
      r.classList.remove('selected', 'kb-focus')
    })
    row.classList.add('selected')
    row.classList.remove('unread') // opening a message marks it \Seen server-side
  })
}
