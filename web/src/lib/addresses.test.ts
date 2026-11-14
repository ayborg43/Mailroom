import { describe, expect, it } from 'vitest'
import { addAddresses, displayLabel, formatBytes, isValidAddress, splitAddresses } from './addresses'

describe('splitAddresses', () => {
  it('splits on commas, semicolons and newlines', () => {
    expect(splitAddresses('a@x.com, b@y.com; c@z.com\nd@w.com')).toEqual([
      'a@x.com',
      'b@y.com',
      'c@z.com',
      'd@w.com',
    ])
  })
  it('keeps commas inside quoted display names', () => {
    expect(splitAddresses('"Doe, Jo" <jo@y.com>, b@y.com')).toEqual(['"Doe, Jo" <jo@y.com>', 'b@y.com'])
  })
  it('keeps commas inside angle brackets', () => {
    expect(splitAddresses('Jo <jo@y.com>,  ')).toEqual(['Jo <jo@y.com>'])
  })
  it('drops empties', () => {
    expect(splitAddresses(' , ;; ')).toEqual([])
  })
})

describe('isValidAddress', () => {
  it('accepts bare and named addresses', () => {
    expect(isValidAddress('a@x.com')).toBe(true)
    expect(isValidAddress('Jo Doe <jo@y.co.uk>')).toBe(true)
    expect(isValidAddress('"Doe, Jo" <jo@y.com>')).toBe(true)
  })
  it('rejects obvious junk', () => {
    expect(isValidAddress('nope')).toBe(false)
    expect(isValidAddress('a@b')).toBe(false)
    expect(isValidAddress('a b@c.com')).toBe(false)
    expect(isValidAddress('<a@b.com')).toBe(false)
  })
})

describe('displayLabel', () => {
  it('prefers the name', () => {
    expect(displayLabel('Jo Doe <jo@y.com>')).toBe('Jo Doe')
    expect(displayLabel('"Doe, Jo" <jo@y.com>')).toBe('Doe, Jo')
  })
  it('falls back to the address', () => {
    expect(displayLabel('<jo@y.com>')).toBe('jo@y.com')
    expect(displayLabel('jo@y.com')).toBe('jo@y.com')
  })
})

describe('addAddresses', () => {
  it('dedupes case-insensitively', () => {
    expect(addAddresses(['A@x.com'], ['a@x.com', 'b@x.com'])).toEqual(['A@x.com', 'b@x.com'])
  })
})

describe('formatBytes', () => {
  it('formats', () => {
    expect(formatBytes(12)).toBe('12 B')
    expect(formatBytes(2048)).toBe('2.0 KB')
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MB')
  })
})
