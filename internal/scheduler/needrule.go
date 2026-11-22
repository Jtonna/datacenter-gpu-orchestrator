package scheduler

// needRow is one stored measurement of a shape, as the need rule sees it:
// the device count it was taken on, its total, and whether it is trusted
// (measured this run, or on the llama build that runs now: trusted).
type needRow struct {
	Devices int
	Total   int
	Trusted bool
}

// needRule is everything needFor decides from; it holds no scheduler state.
type needRule struct {
	// Estimate is the need on k devices without a measurement; it does not
	// decrease with k.
	Estimate func(k int) int
	// Floor: the estimate is a configured needs_mib, which a measurement
	// only ever raises.
	Floor bool
	// PerDevice is the estimator's cost of one more device (0 or more).
	PerDevice int
	Rows      []needRow
}

// needFor is the need of a placement on k devices (k >= 1), and the index
// of the row it comes from (-1: the estimate).
//
// A row's figure for k devices is its total moved by the per-device cost
// for each device more or fewer: total + (k - devices) x PerDevice. Then:
//
//   - Trusted rows decide. Where a trusted row exists on k or more devices,
//     the need is the highest figure of the trusted rows (a measurement
//     replaces the estimate, also when it is lower). With no trusted row on
//     k or fewer devices it is also never below the total measured on the
//     closest trusted count above k, as it was taken: a placement on fewer
//     devices does not take more, so that total is safe where nothing
//     smaller was measured, while the per-device cost taken off it may be
//     larger than the real one. Above the largest
//     trusted count the estimate is a lower bound as well. With no trusted
//     row the need is the estimate.
//   - Untrusted rows only raise: the need is at least each untrusted row's
//     figure.
//   - With Floor, the need is never below the estimate.
//
// Invariants (TestNeedRule_Invariants checks them on random row sets):
//
//	I1. Adding a row never lowers the need for any k, except a trusted row
//	    that is the first measurement nearer k: one on j >= k devices where
//	    no trusted row on k to j devices was known (it replaces the
//	    estimate, or a measurement on more devices), or one on j < k
//	    devices where no trusted row on k or fewer devices was known (it
//	    replaces the total measured on more devices taken as it was).
//	I2. An untrusted row can only raise.
//	I3. The need for k is at least every trusted total measured on k devices.
//	I4. It is at least every trusted total on j < k devices plus (k - j)
//	    per-device costs, and at least every trusted total on j > k devices
//	    less (j - k) per-device costs.
//	I5. The need does not decrease with k.
//	I6. With no rows it is the estimate; with only untrusted rows it is at
//	    least the estimate.
//	I7. It never exceeds the higher of the estimate for k and every row's
//	    total plus the per-device costs of the devices k adds to it.
//
// I1 is narrower than "adding any row never lowers": together with I6 that
// would keep every need at or above the estimate, and a trusted measurement
// below the estimate (an estimate that is too high) would never be used.
func needFor(r needRule, k int) (int, int) {
	pd := max(r.PerDevice, 0)
	moved := func(row needRow) int { return row.Total + (k-row.Devices)*pd }
	fig, from := 0, -1
	take := func(v, i int) {
		if v > fig || (v == fig && from >= 0 && better(r.Rows[i], r.Rows[from], k)) {
			fig, from = v, i
		}
	}
	hi := 0 // the largest trusted count
	for i, row := range r.Rows {
		if !row.Trusted {
			continue
		}
		hi = max(hi, row.Devices)
		take(moved(row), i)
	}
	if hi == 0 || k > hi || r.Floor {
		if est := r.Estimate(k); est >= fig {
			fig, from = est, -1
		}
	}
	if above := closestAbove(r.Rows, k); above > 0 && !trustedUpTo(r.Rows, k) {
		for i, row := range r.Rows {
			if row.Trusted && row.Devices == above {
				take(row.Total, i)
			}
		}
	}
	for i, row := range r.Rows {
		if !row.Trusted {
			take(moved(row), i)
		}
	}
	return fig, from
}

// closestAbove is the smallest trusted count above k, 0 when there is none.
func closestAbove(rows []needRow, k int) int {
	c := 0
	for _, row := range rows {
		if row.Trusted && row.Devices > k && (c == 0 || row.Devices < c) {
			c = row.Devices
		}
	}
	return c
}

// trustedUpTo reports whether a trusted row is on k or fewer devices.
func trustedUpTo(rows []needRow, k int) bool {
	for _, row := range rows {
		if row.Trusted && row.Devices <= k {
			return true
		}
	}
	return false
}

// better breaks a tie between two rows giving the same figure: the one on
// k devices, then a trusted one, then the closer count.
func better(a, b needRow, k int) bool {
	if (a.Devices == k) != (b.Devices == k) {
		return a.Devices == k
	}
	if a.Trusted != b.Trusted {
		return a.Trusted
	}
	return abs(a.Devices-k) < abs(b.Devices-k)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
