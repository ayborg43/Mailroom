// Sizes each HTML-email iframe to its content. (Replaces an inline onload
// attribute, which a Content-Security-Policy without 'unsafe-inline' would
// block.) The frames are same-origin sandboxed (allow-same-origin only, no
// scripts), so we can read their document height.
const MAX_HEIGHT = 8000

function fit(frame: HTMLIFrameElement): void {
  const doc = frame.contentDocument
  if (!doc) return
  // documentElement.scrollHeight is never smaller than the viewport, so it
  // can't shrink a frame; the element's own height can. (An email that sets
  // html{height:100%} just reports the current frame height, which is stable.)
  const content = Math.max(doc.documentElement.offsetHeight, doc.body?.scrollHeight ?? 0)
  const h = Math.min(Math.ceil(content) + 1, MAX_HEIGHT)
  // The tolerance also stops a 1px rounding ratchet on 100%-height emails.
  if (Math.abs(frame.offsetHeight - h) > 2) frame.style.height = `${h}px`
}

function watch(frame: HTMLIFrameElement): void {
  fit(frame)
  const doc = frame.contentDocument
  if (!doc || frame.dataset.watched) return
  frame.dataset.watched = '1'
  // Images and fonts inside the email arrive after load and change its height.
  new ResizeObserver(() => fit(frame)).observe(doc.documentElement)
}

export function fitHtmlFrames(root: ParentNode = document): void {
  root.querySelectorAll<HTMLIFrameElement>('iframe.html-body').forEach((frame) => {
    if (frame.dataset.fitBound) return
    frame.dataset.fitBound = '1'
    frame.addEventListener('load', () => watch(frame))
    // srcdoc frames can finish loading before this runs.
    if (frame.contentDocument?.readyState === 'complete') watch(frame)
  })
}
