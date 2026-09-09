import { Link, Outlet, useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/session'
import { enumLabel } from '../i18n/enums'
import { useLocale } from '../i18n/LocaleProvider'
import { usePrivateFeed } from '../ws/PrivateFeedProvider'

export function Layout() {
  const { session, logout } = useAuth()
  const { status } = usePrivateFeed()
  const { locale, setLocale, t } = useLocale()
  const navigate = useNavigate()
  return (
    <>
      <header className="topbar">
        <Link to="/markets" className="brand">
          Exchange
        </Link>
        <nav>
          <Link to="/markets" data-testid="nav-markets">
            {t('nav.markets')}
          </Link>
          {session && (
            <Link to="/wallet" data-testid="nav-wallet">
              {t('nav.wallet')}
            </Link>
          )}
        </nav>
        <span className="spacer" />
        <span className="tabs lang" role="group" aria-label={t('lang.switch')}>
          <button type="button" className={locale === 'zh-TW' ? 'active' : ''} onClick={() => setLocale('zh-TW')} data-testid="lang-zh-TW" lang="zh-TW">
            中文
          </button>
          <button type="button" className={locale === 'en' ? 'active' : ''} onClick={() => setLocale('en')} data-testid="lang-en" lang="en">
            EN
          </button>
        </span>
        {session ? (
          <span className="row">
            <span className="muted" title={t('nav.private_stream', { status: enumLabel(locale, 'feed', status) })}>
              <span className={`status-dot ${status}`} data-testid="private-status" data-status={status} />
              {session.accountId.slice(0, 8)}…
            </span>
            <button
              onClick={() => {
                void logout().then(() => navigate('/login'))
              }}
            >
              {t('nav.logout')}
            </button>
          </span>
        ) : (
          <span className="row">
            <Link to="/login">{t('nav.login')}</Link>
            <Link to="/register">{t('nav.register')}</Link>
          </span>
        )}
      </header>
      <Outlet />
    </>
  )
}
