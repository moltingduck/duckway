package web

import "embed"

//go:embed templates/*.html static/*
var Content embed.FS
