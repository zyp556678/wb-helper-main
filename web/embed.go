// Package web 把前端构建产物（web/dist）嵌进二进制。
//
// 构建顺序：先在 web/ 下执行 npm run build，再编译 Go。
// 同一份产物同时被浏览器访问与桌面壳加载，不产出两份前端。
package web

import "embed"

// Dist 是面板静态资源：web/dist 下的全部文件。
//
//go:embed all:dist
var Dist embed.FS
