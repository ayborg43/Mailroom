import { defineConfig } from 'vite'
import preact from '@preact/preset-vite'

// The Go server owns every page; Vite only builds (and, in dev, serves) the
// CSS/JS those pages load.
//
// Build:  `npm run build` writes hashed assets plus manifest.json into the
//         directory Go embeds (internal/webmail/static/dist), served at
//         /static/dist/.
// Dev:    run the Go server with MAILSERVER_WEBMAIL_DEV=1 (it then emits
//         Vite's dev-server tags instead of the built files), run `npm run
//         dev`, and browse http://localhost:5173. Vite serves modules and HMR
//         itself and proxies every other request to the Go server, so the page
//         and its session cookie stay same-origin.
const goServer = process.env.MAILSERVER_URL ?? 'http://localhost:8080'

export default defineConfig(({ command }) => ({
  base: command === 'build' ? '/static/dist/' : '/',
  plugins: [preact()],
  build: {
    outDir: '../internal/webmail/static/dist',
    emptyOutDir: true,
    manifest: 'manifest.json',
    rollupOptions: { input: 'src/main.ts' },
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      // Everything except Vite's own URLs goes to the Go server.
      '^(?!/(@|src/|node_modules/|__vite)).*': { target: goServer, changeOrigin: false },
    },
  },
  test: { environment: 'node' },
}))
