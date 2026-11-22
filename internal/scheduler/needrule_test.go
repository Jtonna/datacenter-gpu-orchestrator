package scheduler

import (
	"math/rand"
	"testing"
)

// The need rule (needFor) is tested against its contract, not against a
// second copy of its arithmetic: every expectation below is computed from
// the rows directly, or compares the rule with itself on a changed input.

func ruleEstimate(base, perDevice int, configured bool) func(int) int {
	if configured {
		return func(int) int { return base }
	}
	return func(k int) int { return base + k*perDevice }
}

// randomRows draws n rows on 1 to 6 devices, around and far from the
// estimate, trusted and untrusted mixed.
func randomRows(r *rand.Rand, n, base int) []needRow {
	rows := make([]needRow, n)
	for i := range rows {
		var total int
		switch r.Intn(4) {
		case 0: // far below
			total = base/4 + r.Intn(base/4+1)
		case 1: // far above
			total = base*2 + r.Intn(base*2)
		default: // around
			total = base*7/10 + r.Intn(base*6/10+1)
		}
		rows[i] = needRow{Devices: 1 + r.Intn(6), Total: total, Trusted: r.Intn(2) == 0}
	}
	return rows
}

func randomRule(r *rand.Rand) needRule {
	base := 2000 + r.Intn(30000)
	pd := r.Intn(2500)
	configured := r.Intn(5) == 0
	return needRule{Estimate: ruleEstimate(base, pd, configured), Floor: configured, PerDevice: pd, Rows: randomRows(r, r.Intn(6), base)}
}

func fig(rule needRule, k int) int { f, _ := needFor(rule, k); return f }

func withRow(rule needRule, x needRow) needRule {
	out := rule
	out.Rows = append(append([]needRow(nil), rule.Rows...), x)
	return out
}

// trustedIn reports whether rows hold a trusted row on lo to hi devices.
func trustedIn(rows []needRow, lo, hi int) bool {
	for _, r := range rows {
		if r.Trusted && r.Devices >= lo && r.Devices <= hi {
			return true
		}
	}
	return false
}

func TestNeedRule_Invariants(t *testing.T) {
	const sets = 24000
	checked := map[string]int{}
	for seed := int64(1); seed <= 4; seed++ {
		r := rand.New(rand.NewSource(seed))
		for n := 0; n < sets/4; n++ {
			rule := randomRule(r)
			pd := rule.PerDevice
			trustedRows, untrustedRows := 0, 0
			for _, row := range rule.Rows {
				if row.Trusted {
					trustedRows++
				} else {
					untrustedRows++
				}
			}
			for k := 1; k <= 6; k++ {
				got := fig(rule, k)
				est := rule.Estimate(k)
				// I3, I4 and the larger count (TS1)
				for _, row := range rule.Rows {
					if !row.Trusted {
						// untrusted rows raise as trusted ones do (TB1: a
						// row of another build never switches a bound off)
						if moved := row.Total + (k-row.Devices)*pd; got < moved {
							t.Fatalf("I2: seed %d set %d k=%d: %d below the untrusted %+v moved to %d (%+v)", seed, n, k, got, row, moved, rule.Rows)
						}
						continue
					}
					switch {
					case row.Devices == k && got < row.Total:
						t.Fatalf("I3: seed %d set %d k=%d: %d below the trusted %d-device total %d (%+v)", seed, n, k, got, row.Devices, row.Total, rule.Rows)
					case row.Devices < k && got < row.Total+(k-row.Devices)*pd:
						t.Fatalf("I4: seed %d set %d k=%d: %d below %d on %d devices + %d x %d (%+v)", seed, n, k, got, row.Total, row.Devices, k-row.Devices, pd, rule.Rows)
					case row.Devices > k && got < row.Total-(row.Devices-k)*pd:
						t.Fatalf("TS1: seed %d set %d k=%d: %d below %d on %d devices - %d x %d (%+v)", seed, n, k, got, row.Total, row.Devices, row.Devices-k, pd, rule.Rows)
					}
				}
				// I6 and the configured floor
				if len(rule.Rows) == 0 && got != est {
					t.Fatalf("I6: no rows, k=%d: %d, estimate %d", k, got, est)
				}
				if trustedRows == 0 && got < est {
					t.Fatalf("I6: only untrusted rows, k=%d: %d below the estimate %d (%+v)", k, got, est, rule.Rows)
				}
				if rule.Floor && got < est {
					t.Fatalf("floor: k=%d: %d below the configured %d", k, got, est)
				}
				// I7: no runaway
				bound := est
				for _, row := range rule.Rows {
					bound = max(bound, row.Total+max(k-row.Devices, 0)*pd)
				}
				if got > bound {
					t.Fatalf("I7: seed %d set %d k=%d: %d above %d (%+v)", seed, n, k, got, bound, rule.Rows)
				}
				// I5
				if k < 6 && fig(rule, k+1) < got {
					t.Fatalf("I5: seed %d set %d: %d devices %d, %d devices %d (%+v)", seed, n, k, got, k+1, fig(rule, k+1), rule.Rows)
				}
				// I1 and I2: one more row
				x := randomRows(r, 1, est)[0]
				after := fig(withRow(rule, x), k)
				mayLower := x.Trusted && (x.Devices >= k && !trustedIn(rule.Rows, k, x.Devices) ||
					x.Devices < k && !trustedIn(rule.Rows, 1, k))
				if after < got && !mayLower {
					t.Fatalf("I1/I2: seed %d set %d k=%d: adding %+v lowered %d to %d (%+v)", seed, n, k, x, got, after, rule.Rows)
				}
				switch {
				case len(rule.Rows) >= 2 && trustedRows > 0 && untrustedRows > 0:
					checked["two or more rows, trusted and untrusted"]++
				case untrustedRows > 0:
					checked["untrusted only"]++
				}
			}
			checked["sets"]++
		}
	}
	if checked["sets"] < 20000 || checked["two or more rows, trusted and untrusted"] < 20000 {
		t.Fatalf("too few cases: %v", checked)
	}
	t.Logf("cases: %v", checked)
}

// The reviewer's probes (third pass) and the worked cases of the doc comment.
func TestNeedRule_Cases(t *testing.T) {
	pd := 856
	est := ruleEstimate(7980, pd, false) // 7600 + 5 %
	cases := []struct {
		name string
		rows []needRow
		k    int
		want int
	}{
		{"TB1: trusted 1-device 9000", []needRow{{1, 9000, true}}, 3, 9000 + 2*pd},
		{"TB1: plus an untrusted 3-device 8000", []needRow{{1, 9000, true}, {3, 8000, false}}, 3, 9000 + 2*pd},
		{"TB1: trusted 1-device 11800", []needRow{{1, 11800, true}}, 3, 13512},
		{"TB1: plus an untrusted 3-device 8000", []needRow{{1, 11800, true}, {3, 8000, false}}, 3, 13512},
		{"TS1: between 1 and 3, the larger count bounds", []needRow{{1, 9000, true}, {3, 12000, true}}, 2, 12000 - pd},
		{"TS1: between 1 and 3, the smaller count scaled up", []needRow{{1, 11500, true}, {3, 12000, true}}, 2, 11500 + pd},
		{"a trusted exact row below the estimate is used", []needRow{{3, 8000, true}}, 3, 8000},
		{"an untrusted exact row below the estimate is not", []needRow{{3, 8000, false}}, 3, 7980 + 3*pd},
		{"an untrusted row raises", []needRow{{3, 8000, true}, {1, 9000, false}}, 3, 9000 + 2*pd},
		{"fewer devices than measured: the measured total", []needRow{{3, 14000, true}}, 1, 14000},
		{"more devices than measured: never below the estimate", []needRow{{1, 5000, true}}, 3, 7980 + 3*pd},
	}
	for _, c := range cases {
		if got := fig(needRule{Estimate: est, PerDevice: pd, Rows: c.rows}, c.k); got != c.want {
			t.Errorf("%s: %d devices: %d, want %d", c.name, c.k, got, c.want)
		}
	}
	// a configured needs_mib is a floor: measurements only raise it
	if got := fig(needRule{Estimate: ruleEstimate(9000, 0, true), Floor: true, PerDevice: pd, Rows: []needRow{{2, 7000, true}}}, 2); got != 9000 {
		t.Errorf("configured 9000, trusted 7000: %d", got)
	}
}
