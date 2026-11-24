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

// tuned is a trailing "-it" or "-instruct" (any separator).
var tuned = regexp.MustCompile(`[-_.](?:it|instruct)$`)

// publisher is a leading word of letters only (no digit, no hyphen)
// followed by an underscore: the "org_repo" form of a Hugging Face path
// ("moonshotai_kimi-dev-72b", "thudm_glm-4-32b-0414", "google_gemma-3-4b").
var publisher = regexp.MustCompile(`^[a-z]+_`)

func dropPublisher(s string) string {
	if m := publisher.FindString(s); m != "" && len(s) > len(m) {
		return s[len(m):]
	}
	return s
}

func dropTuned(s string) string { return tuned.ReplaceAllString(s, "") }

// Namer names model files the way the page shows them. From Short it drops,
// in this order:
//  1. a publisher prefix ("moonshotai_kimi-dev-72b" -> "kimi-dev-72b"),
//  2. a trailing "-it" or "-instruct" ("gemma-4-27b-it" -> "gemma-4-27b"),
//
// each unless two files of the catalog would then share one name: those keep
// what that step would drop (both of them). The same model in two
// quantisations shares its name. It is the one rule for /api/models and the
// request rows; the full file name stays in model_file and file.
type Namer struct {
	byPub   map[string]map[string]bool // without the publisher -> the Short names that give it
	byTuned map[string]map[string]bool // without the suffix -> the step-1 names that give it
}

// NewNamer is the namer of a catalog's model files.
func NewNamer(files []string) *Namer {
	n := &Namer{byPub: map[string]map[string]bool{}, byTuned: map[string]map[string]bool{}}
	add := func(m map[string]map[string]bool, k, v string) {
		if m[k] == nil {
			m[k] = map[string]bool{}
		}
		m[k][v] = true
	}
	var shorts []string
	for _, f := range files {
		if sh := Short(f); sh != "" {
			shorts = append(shorts, sh)
			add(n.byPub, dropPublisher(sh), sh)
		}
	}
	for _, sh := range shorts {
		a := n.step(n.byPub, sh, dropPublisher)
		add(n.byTuned, dropTuned(a), a)
	}
	return n
}

// step drops what drop removes from from, unless another catalog name gives
// the same result.
func (n *Namer) step(m map[string]map[string]bool, from string, drop func(string) string) string {
	to := drop(from)
	if to == from || n == nil {
		return to
	}
	for other := range m[to] {
		if other != from {
			return from
		}
	}
	return to
}

// Name is the page's name of file. A file the catalog does not have is
// named against the catalog as it is. A nil Namer (no catalog) drops both.
func (n *Namer) Name(file string) string {
	sh := Short(file)
	if n == nil {
		return dropTuned(dropPublisher(sh))
	}
	a := n.step(n.byPub, sh, dropPublisher)
	return n.step(n.byTuned, a, dropTuned)
}
