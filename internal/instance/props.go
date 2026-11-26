package instance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PropsTimeout bounds one /props read.
const PropsTimeout = 5 * time.Second

// ReadSlotCtx reads the context per slot a llama server runs with: /props
// default_generation_settings.n_ctx (the slot context after the server's
// own cap at the model's trained context, llama.cpp
// tools/server/server-context.cpp n_ctx_slot), or the top-level n_ctx of an
// older server. key, when set, is sent as a bearer token.
func ReadSlotCtx(ctx context.Context, addr, key string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, PropsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/props", nil)
	if err != nil {
		return 0, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("/props answered %d", resp.StatusCode)
	}
	var v struct {
		Default struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
		NCtx int `json:"n_ctx"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&v); err != nil {
		return 0, fmt.Errorf("/props: %w", err)
	}
	n := v.Default.NCtx
	if n <= 0 {
		n = v.NCtx
	}
	if n <= 0 {
		return 0, fmt.Errorf("/props names no n_ctx")
	}
	return n, nil
}
