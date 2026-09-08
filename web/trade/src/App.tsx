import { Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { useAuth } from './auth/session'
import { Layout } from './components/Layout'
import { LoginPage } from './pages/LoginPage'
import { MarketsPage } from './pages/MarketsPage'
import { RegisterPage } from './pages/RegisterPage'
import { TradePage } from './pages/TradePage'
import { WalletPage } from './pages/WalletPage'

function RequireAuth({ children }: { children: React.ReactElement }) {
  const { session, ready } = useAuth()
  const location = useLocation()
  if (!ready) return <p className="page muted">Loading…</p>
  if (!session) return <Navigate to="/login" replace state={{ from: location.pathname }} />
  return children
}

export function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route path="/" element={<Navigate to="/markets" replace />} />
        <Route path="/login" element={<LoginPage />} />
        <Route path="/register" element={<RegisterPage />} />
        <Route path="/markets" element={<MarketsPage />} />
        <Route
          path="/trade/:symbol"
          element={
            <RequireAuth>
              <TradePage />
            </RequireAuth>
          }
        />
        <Route
          path="/wallet"
          element={
            <RequireAuth>
              <WalletPage />
            </RequireAuth>
          }
        />
        <Route path="*" element={<Navigate to="/markets" replace />} />
      </Route>
    </Routes>
  )
}
