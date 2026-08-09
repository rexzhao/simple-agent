import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import { SyncApplicationProvider } from './applicationContext'
import { createSyncApplication } from './sync/applicationComposition'
import './styles.css'

// Keep the graph at application scope so StrictMode does not construct a
// second transport/runtime/replica set. Project navigation, session content,
// and run control are all owned by this application graph.
const syncApplication = createSyncApplication()

// React StrictMode only stops the singleton so it can be mounted again. HMR
// is different: the module is being replaced, so the old graph must be fully
// disposed before the replacement creates its application-scoped graph.
if (import.meta.hot) {
  import.meta.hot.dispose(() => syncApplication.dispose())
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <SyncApplicationProvider application={syncApplication} startOnMount>
      <App />
    </SyncApplicationProvider>
  </StrictMode>,
)
