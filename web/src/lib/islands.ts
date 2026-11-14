// Islands: server-rendered pages mark an element with
//   <div data-island="name" data-props='{"json":"props"}'>…fallback markup…</div>
// and the matching module is loaded on demand and mounted over it. A page
// without a given island never downloads its code (or Preact).
type Mount = (el: HTMLElement, props: any) => void

const registry: Record<string, () => Promise<{ default: Mount }>> = {
  compose: () => import('../islands/compose'),
}

export function mountIslands(root: ParentNode = document): void {
  root.querySelectorAll<HTMLElement>('[data-island]').forEach(async (el) => {
    if (el.dataset.islandMounted) return
    const load = registry[el.dataset.island ?? '']
    if (!load) return
    el.dataset.islandMounted = '1'
    let props: unknown = {}
    try {
      props = JSON.parse(el.dataset.props ?? '{}')
    } catch {
      // Malformed props: leave the server-rendered fallback in place.
      return
    }
    try {
      const mod = await load()
      mod.default(el, props)
    } catch (err) {
      // If the island fails to load, the fallback markup keeps working.
      delete el.dataset.islandMounted
      console.error(`island "${el.dataset.island}" failed to load`, err)
    }
  })
}
