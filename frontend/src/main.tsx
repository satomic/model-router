import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import App from './App'
import { DialogProvider } from './components/Dialog'
import './i18n' // Must run before the first render so t() has a catalog
import './styles.css'

// No `basename`: the console owns the site root. The server answers every unknown path with
// index.html, so a pasted URL such as /config/models boots here and the router takes over.
//
// DialogProvider sits inside BrowserRouter so a dialog opened from a page can be answered by a
// handler that then navigates -- the confirmations on the traces page do exactly that.
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter>
      <DialogProvider>
        <App />
      </DialogProvider>
    </BrowserRouter>
  </StrictMode>,
)
