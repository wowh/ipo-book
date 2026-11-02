// Package web 打包前端静态文件，随程序一起分发，运行时无需外部目录。
package web

import "embed"

// Files 是前端的全部静态文件。
//
//go:embed index.html style.css app.js
var Files embed.FS
