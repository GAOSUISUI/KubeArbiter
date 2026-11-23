import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发期通过 Vite 代理直连本机 port-forward 出来的 arbiter-api，
// 浏览器侧同源，避免额外 CORS 配置；部署时用同域反代即可。
export default defineConfig({
  plugins: [vue()],
  build: {
    // 直接输出到 Go 侧 go:embed 目录，`npm run build` 后 go build 即带上控制台。
    outDir: '../internal/webui/dist',
    emptyOutDir: true
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/healthz': { target: 'http://127.0.0.1:8080', changeOrigin: true }
    }
  }
})
