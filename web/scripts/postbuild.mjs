import { writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

// Vite 的 emptyOutDir 会清空 dist；这里补回占位文件，
// 保证仓库在未构建前端时仍能满足 go:embed 的目录存在性要求。
const here = dirname(fileURLToPath(import.meta.url))
writeFileSync(resolve(here, '../../internal/webui/dist/.gitkeep'), '')
