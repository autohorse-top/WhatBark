package web

import "embed"

// FS holds the embedded single-page frontend: index.html, style.css,
// core.js and the per-block block files (org, wflow, projects, status,
// inbox, servers, sched).
//
//go:embed *.html *.css *.js avatars/*.svg i18n/*.js
var FS embed.FS
