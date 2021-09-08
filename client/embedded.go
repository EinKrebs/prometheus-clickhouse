package client

import "embed"

//go:embed sqlscripts/*
var EmbeddedScripts embed.FS
