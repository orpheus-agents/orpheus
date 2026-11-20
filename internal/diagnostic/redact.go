package diagnostic

import (
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
)

const (
	MaxInputBytes   = 64 << 10
	MaxMessageBytes = 4 << 10
	MaxStderrBytes  = 32 << 10
	maxSecretBytes  = 1 << 20
	redacted        = "[REDACTED]"
)

var (
	ansi          = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))`)
	credential    = regexp.MustCompile(`(?i)((?:[a-z_][a-z0-9_.-]*[_.-])?(?:password|passwd|token|secret|api[_-]?key|authorization)["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\\n])*"|'(?:\\.|[^'\\\n])*'|[^\s,;}]+)`)
	authorization = regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[a-z0-9._~+/=-]+`)
	jwt           = regexp.MustCompile(`\beyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]*`)
	urls          = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>"']+`)
	pem           = regexp.MustCompile(`(?s)-----BEGIN [^-\n]+-----.*?(?:-----END [^-\n]+-----|$)`)
	base64Line    = regexp.MustCompile(`^[a-zA-Z0-9+/=]{32,}$`)
	percentEscape = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
)

// Redactor is owned by one executor connection. Values are only kept in memory;
// old credentials remain protected until the connection is discarded.
// The zero value is ready for use.
type Redactor struct {
	mu       sync.Mutex
	values   map[string]struct{}
	bytes    int
	blocked  bool
	replacer *strings.Replacer
}

func normalize(value string) string {
	value = ansi.ReplaceAllString(strings.ToValidUTF8(value, "�"), "")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
}

func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.blocked {
		return
	}
	if r.values == nil {
		r.values = map[string]struct{}{}
	}
	changed := false
	for _, value := range values {
		// Check before encoding or splitting potentially large input.
		if len(value) > maxSecretBytes {
			r.block()
			return
		}
		for part := range strings.SplitSeq(value, "\n") {
			part = normalize(part)
			if part == "" {
				continue
			}
			encoded, _ := json.Marshal(part)
			for _, variant := range []string{part, string(encoded[1 : len(encoded)-1]), url.QueryEscape(part), url.PathEscape(part)} {
				// Canonicalize after encoding: literal percent escapes in the
				// secret must retain their original bytes when URL-encoded.
				form := percentEscape.ReplaceAllStringFunc(variant, strings.ToUpper)
				if _, exists := r.values[form]; exists {
					continue
				}
				if r.bytes+len(form) > maxSecretBytes {
					r.block()
					return
				}
				r.values[form] = struct{}{}
				r.bytes += len(form)
				changed = true
			}
		}
	}
	if changed {
		keys := make([]string, 0, len(r.values))
		for value := range r.values {
			keys = append(keys, value)
		}
		slices.SortFunc(keys, func(a, b string) int {
			if len(a) != len(b) {
				return len(b) - len(a)
			}
			return strings.Compare(a, b)
		})
		pairs := make([]string, 0, 2*len(keys))
		for _, key := range keys {
			pairs = append(pairs, key, redacted)
		}
		r.replacer = strings.NewReplacer(pairs...)
	}
}

func (r *Redactor) block() {
	r.blocked = true
	r.values, r.replacer = nil, nil
	r.bytes = 0
}

func (r *Redactor) Blocked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.blocked
}

// Clean receives a bounded, complete record. A false result forbids publishing
// any free text for this connection, including previously buffered stderr.
func (r *Redactor) Clean(value string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.blocked || len(value) > MaxInputBytes {
		return "", false
	}
	value = normalize(value)
	value = pem.ReplaceAllString(value, redacted)
	value = urls.ReplaceAllString(value, redacted)
	value = authorization.ReplaceAllString(value, redacted)
	value = credential.ReplaceAllString(value, "${1}"+redacted)
	value = jwt.ReplaceAllString(value, redacted)
	if r.replacer != nil {
		value = percentEscape.ReplaceAllStringFunc(value, strings.ToUpper)
		value = r.replacer.Replace(value)
	}
	return value, true
}
