package logrelay

import (
	"encoding/json"
	"net/http"
	"time"
)

// bucket is a token bucket of bytes. It starts full. tokens can go below
// zero: a request whose length was not declared is charged after it streamed.
type bucket struct {
	tokens float64
	last   time.Time
}

// wait refills the bucket and returns how long until it holds n bytes; 0
// when it holds them now. It takes nothing out.
func (b *bucket) wait(now time.Time, n, rate, burst float64) time.Duration {
	switch {
	case b.last.IsZero():
		b.tokens, b.last = burst, now
	case now.After(b.last):
		b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
		b.last = now
	}
	if b.tokens >= n {
		return 0
	}
	return time.Duration((n - b.tokens) / rate * float64(time.Second))
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(append(b, '\n'))
}
