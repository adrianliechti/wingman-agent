import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

const monacoLang = /monaco-editor\/esm\/vs\/(basic-languages|language)\//

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: '../static',
    emptyOutDir: true,
    rollupOptions: {
      output: {
        entryFileNames: 'assets/[name].js',
        chunkFileNames: (chunk) =>
          monacoLang.test(chunk.facadeModuleId ?? '')
            ? 'assets/lang/[name].js'
            : 'assets/[name].js',
        assetFileNames: 'assets/[name].[ext]',
        manualChunks(id) {
          if (!id.includes('node_modules/monaco-editor/')) return
          if (monacoLang.test(id)) return
          return 'editor'
        },
      },
    },
  },
  worker: {
    rollupOptions: {
      output: {
        entryFileNames: 'assets/workers/[name].js',
        chunkFileNames: 'assets/workers/[name].js',
      },
    },
  },
  server: {
    proxy: {
      '/api': 'http://localhost:4242',
      '/ws': { target: 'ws://localhost:4242', ws: true },
    },
  },
})
