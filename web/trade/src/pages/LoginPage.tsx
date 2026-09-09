import { useState, type FormEvent } from 'react'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import { describeError } from '../api/client'
import { useAuth } from '../auth/session'
import { useLocale } from '../i18n/LocaleProvider'

export function LoginPage() {
  const { login } = useAuth()
  const { t } = useLocale()
  const navigate = useNavigate()
  const location = useLocation()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const from = (location.state as { from?: string } | null)?.from ?? '/markets'

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await login(email, password)
      navigate(from, { replace: true })
    } catch (err) {
      setError(describeError(err, t))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="page narrow">
      <form className="card stack" onSubmit={submit}>
        <h2>{t('auth.login')}</h2>
        <div className="field">
          <label htmlFor="email">{t('auth.email')}</label>
          <input id="email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoComplete="username" data-testid="login-email" />
        </div>
        <div className="field">
          <label htmlFor="password">{t('auth.password')}</label>
          <input id="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="current-password" data-testid="login-password" />
        </div>
        {error && <p className="error" role="alert">{error}</p>}
        <button className="primary" type="submit" disabled={busy} data-testid="login-submit">
          {t('auth.login')}
        </button>
        <p className="muted">
          {t('auth.no_account')} <Link to="/register">{t('auth.register')}</Link>
        </p>
      </form>
    </div>
  )
}
