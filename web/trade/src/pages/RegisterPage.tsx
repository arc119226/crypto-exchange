import { useState, type FormEvent } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { describeError } from '../api/client'
import { useAuth } from '../auth/session'

export function RegisterPage() {
  const { register } = useAuth()
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
      setError(describeError(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="page narrow">
      <form className="card stack" onSubmit={submit}>
        <h2>Register</h2>
        <div className="field">
          <label htmlFor="email">Email</label>
          <input id="email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoComplete="username" />
        </div>
        <div className="field">
          <label htmlFor="password">Password (8+ characters)</label>
          <input id="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} autoComplete="new-password" />
        </div>
        {error && <p className="error" role="alert">{error}</p>}
        <button className="primary" type="submit" disabled={busy}>
          Create account
        </button>
        <p className="muted">
          Have one? <Link to="/login">Log in</Link>
        </p>
      </form>
    </div>
  )
}
