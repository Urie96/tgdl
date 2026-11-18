package main

import (
	"path/filepath"
	"strings"
	"unicode"
)

// maxNameLen caps a single path element so we never hit filesystem limits.
const maxNameLen = 120

var mimeExt = map[string]string{
	"video/mp4":        ".mp4",
	"video/quicktime":  ".mov",
	"video/x-matroska": ".mkv",
	"video/webm":       ".webm",
	"video/x-flv":      ".flv",
	"video/x-msvideo":  ".avi",
	"video/mpeg":       ".mpeg",
	"video/3gpp":       ".3gp",
	"video/x-ms-wmv":   ".wmv",
}

// extFromMime returns a file extension for a video mime type.
func extFromMime(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if e, ok := mimeExt[mime]; ok {
		return e
	}
	if i := strings.Index(mime, ";"); i > 0 {
		return extFromMime(mime[:i])
	}
	if strings.HasPrefix(mime, "video/") {
		return "." + sanitize(strings.TrimPrefix(mime, "video/"))
	}
	return ".mp4"
}

// sanitize makes s safe to use as a single path element.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r == 0:
			b.WriteRune('_')
		case r < 0x20 || r == 0x7f:
			// drop control characters
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.Trim(out, ".")
	out = strings.Join(strings.Fields(out), " ")
	if out == "" {
		out = "unnamed"
	}
	if len(out) > maxNameLen {
		out = shorten(out, maxNameLen)
	}
	return out
}

// shorten truncates s to at most n bytes without splitting a rune.
func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8Start(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	if cut == "" {
		return s[:n]
	}
	return strings.TrimRight(cut, " ")
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// isNumeric reports whether s is a signed decimal integer.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && r == '-' {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// splitExt splits a filename into a sanitized stem and a safe extension.
func splitExt(name string) (stem, ext string) {
	raw := filepath.Ext(name)
	stem = sanitize(strings.TrimSuffix(name, raw))
	if stem == "" {
		stem = "video"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		if r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') {
			b.WriteRune(r)
		}
	}
	ext = b.String()
	if len(ext) > 8 {
		ext = ext[:8]
	}
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return stem, ext
}
