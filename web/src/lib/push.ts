// Web Push opt-in. The account page has a [data-push] block; this module
// shows whether this device gets new-mail notifications and lets the user turn
// them on, off, or send themselves a test. It also switches notifications off
// for a device when its user logs out, so a shared computer doesn't keep
// alerting someone who has signed out.
import { isIOS, isStandalone } from './pwa'

function pushSupported(): boolean {
  return 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window
}

interface Reply<T> {
  ok: boolean
  status: number
  data: T
}

async function postJSON<T = Record<string, unknown>>(url: string, body: unknown): Promise<Reply<T>> {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
    body: JSON.stringify(body),
  })
  let data = {} as T
  try {
    data = (await res.json()) as T
  } catch {
    // Non-JSON body (e.g. a proxy error page): treat as an empty reply.
  }
  return { ok: res.ok, status: res.status, data }
}

function errorText(r: Reply<{ error?: string }>): string {
  if (r.status === 401) return 'Your session expired. Sign in again and retry.'
  return r.data.error ?? `The server refused the request (${r.status}).`
}

// PushManager.subscribe wants the VAPID key as bytes.
function keyBytes(b64url: string): Uint8Array<ArrayBuffer> {
  const pad = '='.repeat((4 - (b64url.length % 4)) % 4)
  const raw = atob((b64url + pad).replace(/-/g, '+').replace(/_/g, '/'))
  const out = new Uint8Array(new ArrayBuffer(raw.length))
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i)
  return out
}

async function currentSubscription(): Promise<PushSubscription | null> {
  if (!pushSupported()) return null
  const reg = await navigator.serviceWorker.getRegistration()
  return reg ? reg.pushManager.getSubscription() : null
}

/** Resolves with the active registration, or rejects if the worker isn't coming. */
function readyRegistration(timeoutMs = 8000): Promise<ServiceWorkerRegistration> {
  return Promise.race([
    navigator.serviceWorker.ready,
    new Promise<never>((_, reject) => setTimeout(() => reject(new Error('The service worker did not start.')), timeoutMs)),
  ])
}

export function initPushSettings(): void {
  const root = document.querySelector<HTMLElement>('[data-push]')
  if (!root) return
  const vapidKey = root.dataset.vapidKey ?? ''
  const status = root.querySelector<HTMLElement>('[data-push-status]')!
  const enableBtn = root.querySelector<HTMLButtonElement>('[data-push-enable]')!
  const disableBtn = root.querySelector<HTMLButtonElement>('[data-push-disable]')!
  const testBtn = root.querySelector<HTMLButtonElement>('[data-push-test]')!
  root.hidden = false

  const show = (text: string, buttons: { enable?: boolean; disable?: boolean; test?: boolean } = {}) => {
    status.textContent = text
    enableBtn.hidden = !buttons.enable
    disableBtn.hidden = !buttons.disable
    testBtn.hidden = !buttons.test
  }
  const busy = (on: boolean) => [enableBtn, disableBtn, testBtn].forEach((b) => (b.disabled = on))

  async function refresh(): Promise<void> {
    if (!pushSupported()) {
      show(
        isIOS() && !isStandalone()
          ? 'On iPhone and iPad, notifications work once Mail is added to your Home Screen: tap Share, then “Add to Home Screen”, and open Mail from there.'
          : 'This browser doesn’t support push notifications.',
      )
      return
    }
    if (Notification.permission === 'denied') {
      show('Notifications are blocked for this site. Allow them in your browser’s site settings, then reload this page.')
      return
    }
    const sub = await currentSubscription()
    if (sub) {
      // Re-register with the signed-in account: the subscription belongs to
      // the browser, which may have been used by someone else before.
      void postJSON('/push/subscribe', sub.toJSON())
      show('Notifications are on for this device.', { disable: true, test: true })
    } else {
      show('Notifications are off for this device.', { enable: true })
    }
  }

  enableBtn.addEventListener('click', async () => {
    busy(true)
    try {
      const permission = await Notification.requestPermission()
      if (permission !== 'granted') {
        show(
          permission === 'denied'
            ? 'Notifications are blocked for this site. Allow them in your browser’s site settings, then reload this page.'
            : 'Notifications weren’t turned on. You can try again any time.',
          permission === 'denied' ? {} : { enable: true },
        )
        return
      }
      const reg = await readyRegistration()
      const options = { userVisibleOnly: true, applicationServerKey: keyBytes(vapidKey) }
      let sub: PushSubscription
      try {
        sub = await reg.pushManager.subscribe(options)
      } catch (err) {
        // A subscription made with a different server key blocks a new one.
        if ((err as DOMException).name !== 'InvalidStateError') throw err
        await (await reg.pushManager.getSubscription())?.unsubscribe()
        sub = await reg.pushManager.subscribe(options)
      }
      const reply = await postJSON<{ error?: string }>('/push/subscribe', sub.toJSON())
      if (!reply.ok) {
        await sub.unsubscribe()
        show(errorText(reply), { enable: true })
        return
      }
      show('Notifications are on for this device.', { disable: true, test: true })
    } catch (err) {
      console.error('enabling push failed', err)
      show('Couldn’t turn on notifications: ' + ((err as Error).message || 'unknown error') + '.', { enable: true })
    } finally {
      busy(false)
    }
  })

  disableBtn.addEventListener('click', async () => {
    busy(true)
    try {
      await unsubscribeThisDevice()
      show('Notifications are off for this device.', { enable: true })
    } finally {
      busy(false)
    }
  })

  testBtn.addEventListener('click', async () => {
    busy(true)
    const previous = status.textContent ?? ''
    try {
      const reply = await postJSON<{ sent?: number; total?: number; error?: string }>('/push/test', {})
      if (!reply.ok) {
        show(errorText(reply), { disable: true, test: true })
      } else if ((reply.data.sent ?? 0) > 0) {
        show('Sent. It should appear in a moment.', { disable: true, test: true })
      } else {
        show('The push service didn’t accept the test. Try turning notifications off and on again.', { enable: true, disable: true, test: true })
      }
    } catch {
      show(previous, { disable: true, test: true })
    } finally {
      busy(false)
    }
  })

  void refresh()
}

/** Stop notifications on this device: tell the server, then drop the browser's subscription. */
async function unsubscribeThisDevice(): Promise<void> {
  const sub = await currentSubscription()
  if (!sub) return
  try {
    await postJSON('/push/unsubscribe', { endpoint: sub.endpoint })
  } catch {
    // Offline or signed out already: the server prunes dead endpoints anyway.
  }
  await sub.unsubscribe().catch(() => false)
}

/** Switch notifications off for this device just before signing out. */
export function initLogoutCleanup(): void {
  if (!pushSupported()) return
  document.addEventListener('submit', (e) => {
    const form = e.target as HTMLFormElement
    if (!form.matches('form[action="/logout"]')) return
    e.preventDefault()
    // Never let cleanup block signing out.
    const timeout = new Promise<void>((resolve) => setTimeout(resolve, 3000))
    void Promise.race([unsubscribeThisDevice(), timeout]).finally(() => form.submit())
  })
}
