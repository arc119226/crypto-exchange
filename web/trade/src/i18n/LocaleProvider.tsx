import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { LOCALES, translate, type Key, type Locale, type Params, type Translate } from './messages'

export type { Key, Locale, Params, Translate } from './messages'

// The language is a per-browser preference: the switcher in the top bar
// writes it to localStorage and the next visit reads it back; a browser
// that has never chosen gets Traditional Chinese when its own language is
// Chinese and English otherwise. Storage can be missing or refuse writes
// (private mode); both are fine, the choice then lasts one page load.
const STORAGE_KEY = 'exchange.lang'

// IntlTag is the locale handed to Intl (times, the chart's axis). Only
// these two well-formed tags are ever used, whatever navigator.language
// says, so a malformed browser tag cannot make formatting throw.
export type IntlTag = 'en-US' | 'zh-TW'

interface LocaleValue {
  locale: Locale
  intl: IntlTag
  setLocale: (l: Locale) => void
  t: Translate
}

const LocaleContext = createContext<LocaleValue | null>(null)

function isLocale(v: unknown): v is Locale {
  return typeof v === 'string' && (LOCALES as readonly string[]).includes(v)
}

function readStored(): Locale | null {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    return isLocale(v) ? v : null
  } catch {
    return null
  }
}

function writeStored(l: Locale): void {
  try {
    localStorage.setItem(STORAGE_KEY, l)
  } catch {
    // private mode without storage: the choice lasts one page load
  }
}

export function detectLocale(): Locale {
  const stored = readStored()
  if (stored) return stored
  const tag = typeof navigator === 'undefined' ? '' : navigator.language || ''
  return /^zh/i.test(tag) ? 'zh-TW' : 'en'
}

export function intlTag(locale: Locale): IntlTag {
  return locale === 'zh-TW' ? 'zh-TW' : 'en-US'
}

export function LocaleProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(detectLocale)

  useEffect(() => {
    document.documentElement.lang = locale
  }, [locale])

  const setLocale = useCallback((l: Locale) => {
    writeStored(l)
    setLocaleState(l)
  }, [])

  const t = useCallback<Translate>((key: Key, params?: Params) => translate(locale, key, params), [locale])

  const value = useMemo<LocaleValue>(() => ({ locale, intl: intlTag(locale), setLocale, t }), [locale, setLocale, t])
  return <LocaleContext.Provider value={value}>{children}</LocaleContext.Provider>
}

export function useLocale(): LocaleValue {
  const v = useContext(LocaleContext)
  if (!v) throw new Error('useLocale outside LocaleProvider')
  return v
}
