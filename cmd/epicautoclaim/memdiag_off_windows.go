//go:build windows && !memdiag

package main

import "net/http"

// These are the measuring hooks of the "memdiag" build (see
// memdiag_windows.go). In a normal build they do nothing.

func memKnobs(*tuning) {}

func memMark(func(string, ...any), string) {}

func memBase(t http.RoundTripper) http.RoundTripper { return t }
