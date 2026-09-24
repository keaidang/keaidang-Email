import path from "node:path"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    port: 5173,
    host: "0.0.0.0",
    proxy: {
      "/api": {
        target: "https://mail.9o.pw",
        changeOrigin: true,
        configure: (proxy) => {
          proxy.on("proxyRes", (proxyRes) => {
            const cookies = proxyRes.headers["set-cookie"]
            if (cookies) {
              proxyRes.headers["set-cookie"] = cookies.map((c) => c.replace(/;\s*Secure/gi, ""))
            }
          })
        },
      },
      "/healthz": "https://mail.9o.pw",
    },
  },
})
