import { render } from 'preact'
import type {
  TargetedClipboardEvent,
  TargetedDragEvent,
  TargetedEvent,
  TargetedKeyboardEvent,
} from 'preact'
import { useEffect, useRef, useState } from 'preact/hooks'
import {
  addAddresses,
  displayLabel,
  formatBytes,
  isValidAddress,
  splitAddresses,
} from '../lib/addresses'

// Mirrors maxComposeRequestSize in internal/webmail/compose_handlers.go. The
// server is still the authority; this just warns before a large upload fails.
const MAX_REQUEST_BYTES = 25 * 1024 * 1024
const AUTOSAVE_DELAY_MS = 800

export interface ComposeProps {
  to: string
  cc: string
  subject: string
  body: string
  inReplyTo: string
  references: string
  error: string
  cancelHref: string
  folder: string // folder of the message being replied to / forwarded
  mode: string // "", "reply", "replyall" or "forward"
}

interface Draft {
  to: string[]
  cc: string[]
  subject: string
  body: string
  savedAt: number
}

function loadDraft(key: string): Draft | null {
  try {
    const raw = localStorage.getItem(key)
    return raw ? (JSON.parse(raw) as Draft) : null
  } catch {
    return null
  }
}

function saveDraft(key: string, draft: Draft | null) {
  try {
    if (draft) localStorage.setItem(key, JSON.stringify(draft))
    else localStorage.removeItem(key)
  } catch {
    // Storage can be full or blocked (private mode); drafts are best-effort.
  }
}

function hasContent(d: Pick<Draft, 'to' | 'cc' | 'subject' | 'body'>): boolean {
  return d.to.length > 0 || d.cc.length > 0 || d.subject.trim() !== '' || d.body.trim() !== ''
}

function Chip(props: { addr: string; onRemove: () => void }) {
  const valid = isValidAddress(props.addr)
  return (
    <span class={`chip${valid ? '' : ' invalid'}`} title={valid ? props.addr : `Not a valid address: ${props.addr}`}>
      <span class="chip-label">{displayLabel(props.addr)}</span>
      <button type="button" aria-label={`Remove ${props.addr}`} onClick={props.onRemove}>
        ×
      </button>
    </span>
  )
}

interface RecipientFieldProps {
  id: string
  label: string
  chips: string[]
  text: string
  onChips: (next: string[]) => void
  onText: (next: string) => void
  inputRef?: { current: HTMLInputElement | null }
}

function RecipientField(p: RecipientFieldProps) {
  const commit = (raw: string) => {
    const parts = splitAddresses(raw)
    if (parts.length > 0) p.onChips(addAddresses(p.chips, parts))
    p.onText('')
  }

  const onKeyDown = (e: TargetedKeyboardEvent<HTMLInputElement>) => {
    const input = e.currentTarget
    const hasText = input.value.trim() !== ''
    if (e.key === 'Enter' || e.key === ',' || e.key === ';') {
      // Enter must never submit the form from a recipient box (that's what
      // the Send button and Ctrl+Enter are for); it just commits the chip.
      e.preventDefault()
      if (hasText) commit(input.value)
    } else if (e.key === 'Tab' && hasText) {
      commit(input.value) // and let focus move on
    } else if (e.key === 'Backspace' && input.value === '' && p.chips.length > 0) {
      p.onChips(p.chips.slice(0, -1))
    }
  }

  const onPaste = (e: TargetedClipboardEvent<HTMLInputElement>) => {
    const text = e.clipboardData?.getData('text') ?? ''
    if (/[,;\n]/.test(text)) {
      e.preventDefault()
      commit(p.text + text)
    }
  }

  return (
    <div class="field">
      <label for={p.id}>{p.label}</label>
      <div class="chips" onClick={(e) => (e.currentTarget.querySelector('input') as HTMLInputElement | null)?.focus()}>
        {p.chips.map((addr) => (
          <Chip key={addr} addr={addr} onRemove={() => p.onChips(p.chips.filter((c) => c !== addr))} />
        ))}
        <input
          id={p.id}
          ref={p.inputRef}
          type="text"
          autocomplete="off"
          autocapitalize="off"
          spellcheck={false}
          value={p.text}
          onInput={(e) => p.onText(e.currentTarget.value)}
          onKeyDown={onKeyDown}
          onPaste={onPaste}
          onBlur={(e) => commit(e.currentTarget.value)}
        />
      </div>
    </div>
  )
}

function Compose(props: ComposeProps) {
  const formRef = useRef<HTMLFormElement>(null)
  const toInputRef = useRef<HTMLInputElement | null>(null)
  const bodyRef = useRef<HTMLTextAreaElement>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)
  // Hidden fields the chips are serialised into. Held by ref because looking
  // them up by name is ambiguous: the visible inputs' ids ("to", "cc") match
  // form.elements.namedItem too, which then returns a list, not an input.
  const toHiddenRef = useRef<HTMLInputElement>(null)
  const ccHiddenRef = useRef<HTMLInputElement>(null)
  const submittingRef = useRef(false)

  // Replies and forwards arrive with the quoted original already in the body;
  // leave room above it to type, and put the caret there.
  const leadIn = props.mode !== '' && props.body !== '' && props.error === '' ? '\n\n' : ''
  const initial = useRef({
    to: splitAddresses(props.to),
    cc: splitAddresses(props.cc),
    subject: props.subject,
    body: leadIn + props.body,
  }).current

  const [to, setTo] = useState(initial.to)
  const [cc, setCc] = useState(initial.cc)
  const [toText, setToText] = useState('')
  const [ccText, setCcText] = useState('')
  const [showCc, setShowCc] = useState(initial.cc.length > 0)
  const [subject, setSubject] = useState(initial.subject)
  const [body, setBody] = useState(initial.body)
  const [files, setFiles] = useState<File[]>([])
  const [dragging, setDragging] = useState(false)
  // Server-side errors (props.error) come with a re-render of the page; this
  // is for problems caught before submitting, and clears once the user edits.
  const [clientError, setError] = useState('')
  const [sending, setSending] = useState(false)
  const [savedAt, setSavedAt] = useState<number | null>(null)
  const [offeredDraft, setOfferedDraft] = useState<Draft | null>(null)

  const draftKey = `mail:draft:${location.pathname}${location.search}`

  const merged = (list: string[], text: string) => addAddresses(list, splitAddresses(text))
  const current = (): Draft => ({
    to: merged(to, toText),
    cc: merged(cc, ccText),
    subject,
    body,
    savedAt: Date.now(),
  })

  const dirty =
    props.error !== '' ||
    JSON.stringify([merged(to, toText), merged(cc, ccText), subject, body]) !==
      JSON.stringify([initial.to, initial.cc, initial.subject, initial.body])

  // Offer a previously autosaved draft for this same compose context.
  useEffect(() => {
    const saved = loadDraft(draftKey)
    if (saved && props.error === '' && hasContent(saved)) {
      const same =
        JSON.stringify([saved.to, saved.cc, saved.subject, saved.body]) ===
        JSON.stringify([initial.to, initial.cc, initial.subject, initial.body])
      if (!same) setOfferedDraft(saved)
    }
    // Initial focus: recipients when empty, otherwise straight into the body.
    if (initial.to.length === 0) {
      toInputRef.current?.focus()
    } else if (bodyRef.current) {
      bodyRef.current.focus()
      bodyRef.current.setSelectionRange(0, 0)
      bodyRef.current.scrollTop = 0
    }
  }, [])

  useEffect(() => setError(''), [to, cc, toText, ccText, files, body, subject])

  // Debounced autosave to localStorage.
  useEffect(() => {
    if (!dirty || offeredDraft) return
    const t = setTimeout(() => {
      const d = current()
      if (hasContent(d)) {
        saveDraft(draftKey, d)
        setSavedAt(d.savedAt)
      }
    }, AUTOSAVE_DELAY_MS)
    return () => clearTimeout(t)
  }, [to, cc, toText, ccText, subject, body, offeredDraft])

  // Warn before leaving with unsent work (not when we're submitting).
  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (dirty && !submittingRef.current) e.preventDefault()
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => window.removeEventListener('beforeunload', onBeforeUnload)
  })

  const totalBytes = files.reduce((n, f) => n + f.size, 0) + new Blob([body]).size
  const overLimit = totalBytes > MAX_REQUEST_BYTES

  const addFiles = (list: FileList | File[]) => {
    const incoming = Array.from(list)
    setFiles((prev) => {
      const key = (f: File) => `${f.name}:${f.size}:${f.lastModified}`
      const have = new Set(prev.map(key))
      return [...prev, ...incoming.filter((f) => !have.has(key(f)))]
    })
  }

  const onDrop = (e: TargetedDragEvent<HTMLDivElement>) => {
    e.preventDefault()
    setDragging(false)
    if (e.dataTransfer?.files.length) addFiles(e.dataTransfer.files)
  }

  const onSubmit = (e: TargetedEvent<HTMLFormElement, Event>) => {
    e.preventDefault()
    const form = e.currentTarget
    const finalTo = merged(to, toText)
    const finalCc = merged(cc, ccText)

    if (finalTo.length === 0) return setError('Add at least one recipient.')
    if ([...finalTo, ...finalCc].some((a) => !isValidAddress(a))) {
      return setError('Fix or remove the highlighted addresses.')
    }
    if (overLimit) return setError('Message too large (max 25 MB including attachments).')

    // Hand the chips and picked files to a plain multipart form post, so the
    // server handler (and its error re-rendering) works exactly as it does
    // without JavaScript.
    if (toHiddenRef.current) toHiddenRef.current.value = finalTo.join(', ')
    if (ccHiddenRef.current) ccHiddenRef.current.value = finalCc.join(', ')
    const dt = new DataTransfer()
    files.forEach((f) => dt.items.add(f))
    if (fileInputRef.current) fileInputRef.current.files = dt.files

    setError('')
    setSending(true)
    submittingRef.current = true
    saveDraft(draftKey, null)
    form.submit()
  }

  const restore = (d: Draft) => {
    setTo(d.to)
    setCc(d.cc)
    setShowCc(d.cc.length > 0)
    setSubject(d.subject)
    setBody(d.body)
    setOfferedDraft(null)
  }
  const discard = () => {
    saveDraft(draftKey, null)
    setOfferedDraft(null)
  }

  const onKeyDown = (e: TargetedKeyboardEvent<HTMLFormElement>) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      formRef.current?.requestSubmit()
    }
  }

  return (
    <div class="compose-page">
      <h1>{props.mode === 'forward' ? 'Forward' : props.mode ? 'Reply' : 'New message'}</h1>
      {(clientError || props.error) && (
        <div class="error-msg" role="alert">
          {clientError || props.error}
        </div>
      )}
      {offeredDraft && (
        <div class="draft-banner">
          <span>You have an unsent draft from {new Date(offeredDraft.savedAt).toLocaleString()}.</span>
          <span class="spacer" />
          <button type="button" class="link-btn" onClick={() => restore(offeredDraft)}>
            Restore
          </button>
          <button type="button" class="link-btn" onClick={discard}>
            Discard
          </button>
        </div>
      )}
      <form
        ref={formRef}
        method="post"
        action="/compose"
        enctype="multipart/form-data"
        onSubmit={onSubmit}
        onKeyDown={onKeyDown}
      >
        <input type="hidden" name="in_reply_to" value={props.inReplyTo} />
        <input type="hidden" name="references" value={props.references} />
        <input type="hidden" name="folder" value={props.folder} />
        <input type="hidden" name="mode" value={props.mode} />
        <input ref={toHiddenRef} type="hidden" name="to" />
        <input ref={ccHiddenRef} type="hidden" name="cc" />

        <RecipientField
          id="to"
          label="To"
          chips={to}
          text={toText}
          onChips={setTo}
          onText={setToText}
          inputRef={toInputRef}
        />
        {showCc ? (
          <RecipientField id="cc" label="Cc" chips={cc} text={ccText} onChips={setCc} onText={setCcText} />
        ) : (
          <p class="field-hint" style="margin: -6px 0 14px">
            <button type="button" class="link-btn" onClick={() => setShowCc(true)}>
              Add Cc
            </button>
          </p>
        )}

        <div class="field">
          <label for="subject">Subject</label>
          <input
            type="text"
            id="subject"
            name="subject"
            value={subject}
            onInput={(e) => setSubject(e.currentTarget.value)}
          />
        </div>
        <div class="field">
          <label for="body">Message</label>
          <textarea
            id="body"
            name="body"
            ref={bodyRef}
            rows={16}
            value={body}
            onInput={(e) => setBody(e.currentTarget.value)}
          />
        </div>

        <div class="field">
          <label>Attachments</label>
          <div
            class={`dropzone${dragging ? ' over' : ''}`}
            onDragOver={(e) => {
              e.preventDefault()
              setDragging(true)
            }}
            onDragLeave={() => setDragging(false)}
            onDrop={onDrop}
          >
            Drop files here or{' '}
            <button type="button" class="link-btn" onClick={() => document.getElementById('compose-browse')?.click()}>
              browse
            </button>
          </div>
          <input
            id="compose-browse"
            type="file"
            multiple
            hidden
            onChange={(e) => {
              if (e.currentTarget.files) addFiles(e.currentTarget.files)
              e.currentTarget.value = ''
            }}
          />
          {/* Populated from `files` at submit time; the server reads this field. */}
          <input ref={fileInputRef} type="file" name="attachments" multiple hidden />
          {files.length > 0 && (
            <ul class="file-list">
              {files.map((f) => (
                <li key={`${f.name}:${f.size}:${f.lastModified}`}>
                  <span class="file-name">{f.name}</span>
                  <span class="file-size">{formatBytes(f.size)}</span>
                  <button
                    type="button"
                    class="link-btn"
                    aria-label={`Remove ${f.name}`}
                    onClick={() => setFiles(files.filter((x) => x !== f))}
                  >
                    Remove
                  </button>
                </li>
              ))}
            </ul>
          )}
          {(files.length > 0 || overLimit) && (
            <div class={`file-total${overLimit ? ' over-limit' : ''}`}>
              {formatBytes(totalBytes)} of 25 MB{overLimit ? ' — too large to send' : ''}
            </div>
          )}
        </div>

        <div class="compose-actions">
          <button type="submit" class="btn btn-primary" disabled={sending || overLimit}>
            {sending ? 'Sending…' : 'Send'}
          </button>
          <a class="btn" href={props.cancelHref}>
            Cancel
          </a>
          <span class="spacer" />
          <span class="compose-status" aria-live="polite">
            {savedAt ? `Draft saved ${new Date(savedAt).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })}` : 'Ctrl+Enter to send'}
          </span>
        </div>
      </form>
    </div>
  )
}

export default function mount(el: HTMLElement, props: ComposeProps) {
  // The server renders a plain working form inside the island root as the
  // no-JavaScript fallback; the island takes its place.
  el.replaceChildren()
  render(<Compose {...props} />, el)
}
