import { useState, type FormEvent } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { describeError } from '../api/client'
import { useAuth } from '../auth/session'
import { useLocale } from '../i18n/LocaleProvider'

export function RegisterPage() {
  const { register } = useAuth()
  const { t } = useLocale()
  const navigate = useNavigate()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await register(email, password)
      navigate('/markets', { replace: true })
    } catch (err) {
      setError(describeError(err, t))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="page narrow">
      <form className="card stack" onSubmit={submit}>
        <h2>{t('auth.register')}</h2>
        <div className="field">
          <label htmlFor="email">{t('auth.email')}</label>
          <input id="email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoComplete="username" data-testid="register-email" />
        </div>
        <div className="field">
          <label htmlFor="password">{t('auth.password_hint')}</label>
          <input id="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} autoComplete="new-password" data-testid="register-password" />
        </div>
        {error && <p className="error" role="alert">{error}</p>}
        <button className="primary" type="submit" disabled={busy} data-testid="register-submit">
          {t('auth.create_account')}
        </button>
        <p className="muted">
          {t('auth.have_account')} <Link to="/login">{t('auth.login')}</Link>
        </p>
      </form>
    </div>
  )
}
