package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Tokenizer counts text in the loaded model's own tokens. HTTPAPI has it
// (the server's /tokenize through the scheduler's upstream passthrough); a
// scripted API without it fails a context_fill phase with a plain error.
type Tokenizer interface {
	// Tokenize answers how many tokens the text is for the profile's model
	// (no special tokens added). The profile must be loaded.
	Tokenize(ctx context.Context, profile, text string) (int, error)
}

// Tokenize implements Tokenizer: POST /upstream/<profile>/tokenize.
func (h *HTTPAPI) Tokenize(ctx context.Context, profile, text string) (int, error) {
	b, _ := json.Marshal(map[string]any{"content": text, "add_special": false})
	resp, err := h.do(ctx, "POST", "/upstream/"+url.PathEscape(profile)+"/tokenize", b, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("/tokenize answered %d: %s", resp.StatusCode, Head(string(raw), 300))
	}
	var t struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return 0, fmt.Errorf("/tokenize: %v", err)
	}
	return len(t.Tokens), nil
}
