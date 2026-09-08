import { Link, Outlet, useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/session'
import { usePrivateFeed } from '../ws/PrivateFeedProvider'

export function Layout() {
  const { session, logout } = useAuth()
  const { status } = usePrivateFeed()
  const navigate = useNavigate()
  return (
    <>
      <header className="topbar">
        <Link to="/markets" className="brand">
          Exchange
        </Link>
        <nav>
          <Link to="/markets">Markets</Link>
          {session && <Link to="/wallet">Wallet</Link>}
        </nav>
        <span className="spacer" />
        {session ? (
          <span className="row">
            <span className="muted" title={`private stream: ${status}`}>
              <span className={`status-dot ${status}`} data-testid="private-status" data-status={status} />
              {session.accountId.slice(0, 8)}…
            </span>
            <button
              onClick={() => {
                void logout().then(() => navigate('/login'))
              }}
            >
              Log out
            </button>
          </span>
        ) : (
          <span className="row">
            <Link to="/login">Log in</Link>
            <Link to="/register">Register</Link>
          </span>
        )}
      </header>
      <Outlet />
    </>
  )
}
