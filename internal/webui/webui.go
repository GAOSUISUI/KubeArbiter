package webui

import "embed"

// Dist 是 Vue 前端构建产物。`npm run build` 会把 dist 直接输出到本目录，
// go:embed 随后把它打进 arbiter-api 二进制，集群内同一个镜像即可托管控制台。
//
// 仓库里保留 dist/.gitkeep，保证未构建前端时 go build 依然可编译；
// 此时路由层检测不到 index.html，会保持纯 API 行为。
//
//go:embed all:dist
var Dist embed.FS
