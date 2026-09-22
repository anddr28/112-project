import react from '@vitejs/plugin-react'
import { defineConfig, loadEnv } from 'vite'

// https://vite.dev/config/
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  // Режим с backend (VITE_USE_MOCKS=false): /api и WebSocket проксируются на go-core.
  const target = env.VITE_API_PROXY ?? 'https://localhost:8443'

  return {
    plugins: [react()],
    server: {
      proxy: {
        '/api': { target, changeOrigin: true, secure: false, ws: true },
      },
    },
  }
})
