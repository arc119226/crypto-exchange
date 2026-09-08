import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { App } from './App'
import { AuthProvider } from './auth/session'
import { LocaleProvider } from './i18n/LocaleProvider'
import { PrivateFeedProvider } from './ws/PrivateFeedProvider'
import './styles.css'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter>
      <LocaleProvider>
        <AuthProvider>
          <PrivateFeedProvider>
            <App />
          </PrivateFeedProvider>
        </AuthProvider>
      </LocaleProvider>
    </BrowserRouter>
  </StrictMode>,
)
