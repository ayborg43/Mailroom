// Exercises internal/webmail/sw.js in a simulated service-worker scope. Real
// browsers can't be driven from here, so this checks the worker's logic
// (caching rules, offline fallback, push display, notification clicks)
// against fakes of the APIs it uses.
import { readFileSync } from 'node:fs'
import { describe, expect, it, vi } from 'vitest'

type Handler = (e: any) => void

function loadWorker(opts: { precache?: string[]; failAdd?: string[] } = {}) {
  const precache = opts.precache ?? ['/offline', '/static/dist/assets/main-abc.js']
  const src = readFileSync(new URL('../../internal/webmail/sw.js', import.meta.url), 'utf8')
    .replaceAll('__VERSION__', 'v1')
    .replaceAll('__PRECACHE__', JSON.stringify(precache))

  const handlers: Record<string, Handler> = {}
  const store = new Map<string, Map<string, Response>>()
  const cacheFor = (name: string) => {
    if (!store.has(name)) store.set(name, new Map())
    const m = store.get(name)!
    return {
      add: async (u: string) => {
        if (opts.failAdd?.includes(u)) throw new Error('404')
        m.set(new URL(u, 'https://mail.example').pathname, new Response('cached:' + u))
      },
      put: async (req: Request, res: Response) => void m.set(new URL(req.url).pathname, res),
    }
  }
  const caches = {
    open: async (name: string) => cacheFor(name),
    keys: async () => [...store.keys()],
    delete: async (name: string) => store.delete(name),
    match: async (req: Request | string) => {
      const path = new URL(typeof req === 'string' ? req : req.url, 'https://mail.example').pathname
      for (const m of store.values()) if (m.has(path)) return m.get(path)!.clone()
      return undefined
    },
  }

  const self = {
    location: new URL('https://mail.example/sw.js'),
    addEventListener: (type: string, fn: Handler) => void (handlers[type] = fn),
    skipWaiting: vi.fn(async () => {}),
    clients: { claim: vi.fn(async () => {}), matchAll: vi.fn(async () => [] as any[]), openWindow: vi.fn(async () => {}) },
    registration: {
      showNotification: vi.fn(async () => {}),
      pushManager: { subscribe: vi.fn(async () => ({ toJSON: () => ({ endpoint: 'https://push.example/new' }) })) },
    },
  }
  const fetchMock = vi.fn()
  new Function('self', 'caches', 'fetch', src)(self, caches, fetchMock)

  /** Dispatch an event and wait for everything it handed to waitUntil/respondWith. */
  async function fire(type: string, props: Record<string, unknown> = {}) {
    const pending: Promise<unknown>[] = []
    let responded: Promise<Response> | undefined
    const event = {
      ...props,
      waitUntil: (p: Promise<unknown>) => void pending.push(p),
      respondWith: (p: Promise<Response>) => void (responded = Promise.resolve(p)),
    }
    handlers[type]?.(event)
    await Promise.all(pending)
    return { responded: responded ? await responded : undefined, handled: responded !== undefined }
  }

  return { self, caches, store, fetchMock, fire, handlers }
}

const req = (url: string, init: { method?: string; mode?: string } = {}) => ({
  url: new URL(url, 'https://mail.example').href,
  method: init.method ?? 'GET',
  mode: init.mode ?? 'cors',
})

describe('install and activate', () => {
  it('precaches the listed files and takes over immediately', async () => {
    const w = loadWorker()
    await w.fire('install')
    expect([...w.store.get('mail-static-v1')!.keys()].sort()).toEqual(['/offline', '/static/dist/assets/main-abc.js'])
    expect(w.self.skipWaiting).toHaveBeenCalled()
  })

  it('still installs when one precache file is missing (so push keeps working)', async () => {
    const w = loadWorker({ failAdd: ['/static/dist/assets/main-abc.js'] })
    await expect(w.fire('install')).resolves.toBeDefined()
    expect(w.self.skipWaiting).toHaveBeenCalled()
    expect([...w.store.get('mail-static-v1')!.keys()]).toEqual(['/offline'])
  })

  it('removes old Mail caches but leaves unrelated ones alone', async () => {
    const w = loadWorker()
    await w.fire('install')
    w.store.set('mail-static-old', new Map())
    w.store.set('something-else', new Map())
    await w.fire('activate')
    expect([...w.store.keys()].sort()).toEqual(['mail-static-v1', 'something-else'])
    expect(w.self.clients.claim).toHaveBeenCalled()
  })
})

describe('fetch', () => {
  it('serves static assets cache-first, and caches new ones from the network', async () => {
    const w = loadWorker()
    await w.fire('install')

    const hit = await w.fire('fetch', { request: req('/static/dist/assets/main-abc.js') })
    expect(await hit.responded!.text()).toBe('cached:/static/dist/assets/main-abc.js')
    expect(w.fetchMock).not.toHaveBeenCalled()

    w.fetchMock.mockResolvedValue(new Response('fresh', { status: 200 }))
    const miss = await w.fire('fetch', { request: req('/static/icons/other.png') })
    expect(await miss.responded!.text()).toBe('fresh')
    expect(w.store.get('mail-static-v1')!.has('/static/icons/other.png')).toBe(true)
  })

  it('does not cache failed static responses', async () => {
    const w = loadWorker()
    await w.fire('install')
    w.fetchMock.mockResolvedValue(new Response('nope', { status: 404 }))
    await w.fire('fetch', { request: req('/static/dist/assets/gone.js') })
    expect(w.store.get('mail-static-v1')!.has('/static/dist/assets/gone.js')).toBe(false)
  })

  it('passes page loads through to the network when online', async () => {
    const w = loadWorker()
    await w.fire('install')
    w.fetchMock.mockResolvedValue(new Response('inbox html'))
    const r = await w.fire('fetch', { request: req('/mail/INBOX', { mode: 'navigate' }) })
    expect(await r.responded!.text()).toBe('inbox html')
    // Mail pages must never be written to a cache.
    expect([...w.store.get('mail-static-v1')!.keys()]).not.toContain('/mail/INBOX')
  })

  it('shows the offline page when a page load fails', async () => {
    const w = loadWorker()
    await w.fire('install')
    w.fetchMock.mockRejectedValue(new TypeError('network down'))
    const r = await w.fire('fetch', { request: req('/mail/INBOX', { mode: 'navigate' }) })
    expect(await r.responded!.text()).toBe('cached:/offline')
  })

  it('leaves everything else alone', async () => {
    const w = loadWorker()
    await w.fire('install')
    for (const request of [
      req('/compose', { method: 'POST', mode: 'navigate' }), // not GET
      req('/mail/INBOX/5', { mode: 'cors' }), // htmx partial
      req('/mail/INBOX/5/attachment/1', { mode: 'no-cors' }), // download
      req('https://elsewhere.example/x.js'), // cross-origin
    ]) {
      expect((await w.fire('fetch', { request })).handled).toBe(false)
    }
  })
})

describe('push', () => {
  it('shows a notification from the payload', async () => {
    const w = loadWorker()
    await w.fire('push', { data: { json: () => ({ title: 'Grace Hopper', body: 'New message', tag: 'new-mail', url: '/mail/INBOX' }) } })
    expect(w.self.registration.showNotification).toHaveBeenCalledWith(
      'Grace Hopper',
      expect.objectContaining({
        body: 'New message',
        tag: 'new-mail',
        renotify: true,
        icon: '/static/icons/icon-192.png',
        badge: '/static/icons/badge-96.png',
        data: { url: '/mail/INBOX' },
      }),
    )
  })

  it('always shows something, even for an empty or malformed payload', async () => {
    for (const data of [undefined, { json: () => { throw new Error('not json') } }]) {
      const w = loadWorker()
      await w.fire('push', { data })
      expect(w.self.registration.showNotification).toHaveBeenCalledTimes(1)
      expect(w.self.registration.showNotification).toHaveBeenCalledWith('New mail', expect.objectContaining({ data: { url: '/mail/INBOX' } }))
    }
  })
})

describe('notification click', () => {
  const click = (url?: string) => ({ notification: { close: vi.fn(), data: url ? { url } : undefined } })

  it('focuses an open Mail window and navigates it', async () => {
    const w = loadWorker()
    const win = { url: 'https://mail.example/mail/Sent', focus: vi.fn(async () => {}), navigate: vi.fn(async () => {}) }
    w.self.clients.matchAll.mockResolvedValue([{ url: 'https://other.example/', focus: vi.fn() }, win])
    const ev = click('/mail/INBOX')
    await w.fire('notificationclick', ev)
    expect(ev.notification.close).toHaveBeenCalled()
    expect(win.focus).toHaveBeenCalled()
    expect(win.navigate).toHaveBeenCalledWith('https://mail.example/mail/INBOX')
    expect(w.self.clients.openWindow).not.toHaveBeenCalled()
  })

  it('opens a new window when none is open', async () => {
    const w = loadWorker()
    w.self.clients.matchAll.mockResolvedValue([])
    await w.fire('notificationclick', click('/mail/INBOX'))
    expect(w.self.clients.openWindow).toHaveBeenCalledWith('https://mail.example/mail/INBOX')
  })

  it('falls back to the inbox without a url', async () => {
    const w = loadWorker()
    w.self.clients.matchAll.mockResolvedValue([])
    await w.fire('notificationclick', click())
    expect(w.self.clients.openWindow).toHaveBeenLastCalledWith('https://mail.example/mail/INBOX')
  })

  it('never navigates off-site, whatever the payload says', async () => {
    const w = loadWorker()
    w.self.clients.matchAll.mockResolvedValue([])
    await w.fire('notificationclick', click('https://evil.example/phish'))
    expect(w.self.clients.openWindow).toHaveBeenLastCalledWith('https://mail.example/mail/INBOX')
    await w.fire('notificationclick', click('//evil.example/phish'))
    expect(w.self.clients.openWindow).toHaveBeenLastCalledWith('https://mail.example/mail/INBOX')
  })
})

describe('pushsubscriptionchange', () => {
  it('resubscribes with the same key and tells the server', async () => {
    const w = loadWorker()
    const fetched = vi.fn(async () => new Response('{}'))
    // sw.js calls the global fetch; the harness passes fetchMock.
    w.fetchMock.mockImplementation(fetched)
    const key = new Uint8Array([1, 2, 3])
    await w.fire('pushsubscriptionchange', { oldSubscription: { options: { applicationServerKey: key } } })
    expect(w.self.registration.pushManager.subscribe).toHaveBeenCalledWith({ userVisibleOnly: true, applicationServerKey: key })
    expect(fetched).toHaveBeenCalledWith('/push/subscribe', expect.objectContaining({ method: 'POST', credentials: 'same-origin' }))
  })

  it('does nothing without the old key', async () => {
    const w = loadWorker()
    await w.fire('pushsubscriptionchange', { oldSubscription: null })
    expect(w.self.registration.pushManager.subscribe).not.toHaveBeenCalled()
  })
})
