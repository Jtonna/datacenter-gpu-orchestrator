// Package modelname is the short name of a model file, the one rule the
// server and the page share (the page's fileShort in
// internal/api/ui/next/core/ui.js).
package modelname

import (
	"regexp"
	"strings"
)

var (
	// the quantisation in a file name: Q4_K_M, IQ3_XS, BF16, F16, F32
	quant   = regexp.MustCompile(`(?i)(^|[-_.])((?:IQ|Q)\d+(?:_[A-Z0-9]+)*|BF16|F16|F32)($|[-_.])`)
	seps    = regexp.MustCompile(`[-_.]{2,}`)
	vendor  = regexp.MustCompile(`(?i)^([a-z]+)[-_]`)
	ggufExt = regexp.MustCompile(`(?i)\.gguf$`)
)

// Short is the file name without its directory, the .gguf extension and the
// quantisation, without a leading vendor word the next word repeats, in lower
// case: "/models/Qwen_Qwen3.5-9B-Q4_K_M.gguf" -> "qwen3.5-9b". It keeps a
// publisher prefix that is not repeated ("google_gemma-3-4b-it") and the
// "-it" or "-instruct" the file carries. "" when nothing is left.
func Short(file string) string {
	s := file
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = ggufExt.ReplaceAllString(s, "")
	if m := quant.FindStringSubmatchIndex(s); m != nil {
		s = s[:m[0]] + s[m[2]:m[3]] + s[m[6]:m[7]] + s[m[1]:]
	}
	s = seps.ReplaceAllStringFunc(s, func(m string) string { return m[:1] })
	s = strings.Trim(s, "-_.")
	if v := vendor.FindStringSubmatch(s); v != nil && strings.HasPrefix(strings.ToLower(s[len(v[0]):]), strings.ToLower(v[1])) {
		s = s[len(v[0]):]
	}
	return strings.ToLower(s)
}
