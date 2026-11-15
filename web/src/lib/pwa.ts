// PWA plumbing: register the service worker (served by Go at /sw.js) and
// drive the "Install app" button on the account page.
//
// The worker caches only static assets and an offline page, and shows push
// notifications — see internal/webmail/sw.js.

export function registerServiceWorker(): void {
  // Vite's dev server serves modules unbundled with no hashed asset list, so
  // skip the worker there; it's tested against real builds.
  if (!('serviceWorker' in navigator) || import.meta.env.DEV) return
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch((err) => {
      console.warn('service worker registration failed', err)
    })
  })
}

// Chromium fires beforeinstallprompt when the app is installable; we hold on
// to it so the account page can offer a real button. (Safari has no such
// event: iPhones install via Share → Add to Home Screen.)
interface InstallPromptEvent extends Event {
  prompt(): Promise<void>
  userChoice: Promise<{ outcome: 'accepted' | 'dismissed' }>
}

export function isStandalone(): boolean {
  return (
    window.matchMedia('(display-mode: standalone)').matches ||
    (navigator as Navigator & { standalone?: boolean }).standalone === true
  )
}

export function isIOS(): boolean {
  return /iphone|ipad|ipod/i.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)
}

export function initInstallPrompt(): void {
  const root = document.querySelector<HTMLElement>('[data-install]')
  if (!root || isStandalone()) return
  const button = root.querySelector<HTMLButtonElement>('[data-install-button]')
  const hint = root.querySelector<HTMLElement>('[data-install-hint]')
  let deferred: InstallPromptEvent | null = null

  window.addEventListener('beforeinstallprompt', (e) => {
    e.preventDefault()
    deferred = e as InstallPromptEvent
    root.hidden = false
    if (button) button.hidden = false
    if (hint) hint.hidden = true
  })

  button?.addEventListener('click', async () => {
    if (!deferred) return
    await deferred.prompt()
    await deferred.userChoice
    deferred = null
    root.hidden = true
  })

  window.addEventListener('appinstalled', () => {
    root.hidden = true
  })

  if (isIOS()) {
    root.hidden = false
    if (button) button.hidden = true
    if (hint) hint.hidden = false
  }
}
