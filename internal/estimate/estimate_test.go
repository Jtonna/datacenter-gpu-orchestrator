package estimate

import "testing"

func TestKVBytesPerElement(t *testing.T) {
	for _, c := range []struct {
		q       string
		want    float64
		wantErr bool
	}{
		{"f16", 2, false}, {"q8_0", 1.0625, false}, {"q4_0", 0.5625, false},
		{"", 0, true}, {"F16", 0, true}, {"q5_0", 0, true},
	} {
		got, err := KVBytesPerElement(c.q)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%q: got %v, %v", c.q, got, err)
		}
	}
}

func TestKVMiB(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		name              string
		layers, ctx, k, v int
		quant             string
		want              int
		wantErr           bool
	}{
		{"exact f16", 32, 8192, 1024, 1024, "f16", 32 * 8192 * 2048 * 2 / mib, false},
		{"q8 exact", 32, 8192, 1024, 1024, "q8_0", 32 * 8192 * 2048 * 17 / 16 / mib, false},
		{"q4 exact", 32, 8192, 1024, 1024, "q4_0", 32 * 8192 * 2048 * 9 / 16 / mib, false},
		{"rounds up by one byte", 1, 1, 1, 524288, "f16", 2, false}, // 1 MiB + 2 bytes
		{"tiny rounds to 1", 1, 1, 1, 1, "f16", 1, false},
		{"zero is zero", 0, 8192, 1, 1, "f16", 0, false},
		{"bad quant", 1, 1, 1, 1, "x", 0, true},
		{"negative", -1, 1, 1, 1, "f16", 0, true},
		{"overflow", 1 << 30, 1 << 30, 1 << 30, 1 << 30, "f16", 0, true},
	}
	for _, c := range cases {
		got, err := KVMiB(c.layers, c.ctx, c.k, c.v, c.quant)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: got %d, %v want %d (err=%v)", c.name, got, err, c.want, c.wantErr)
		}
	}
}

func TestNeedMiB(t *testing.T) {
	const mib = 1 << 20
	for _, c := range []struct {
		size int64
		kv   int
		want int
	}{
		{0, 0, 0}, {1, 0, 1}, {mib, 0, 1}, {mib + 1, 0, 2}, {5 * mib, 100, 105},
	} {
		if got := NeedMiB(c.size, c.kv); got != c.want {
			t.Errorf("NeedMiB(%d,%d)=%d want %d", c.size, c.kv, got, c.want)
		}
	}
}

func TestFitsEstimatedBoundary(t *testing.T) {
	// need 100 => required 105 exactly.
	cases := []struct {
		name    string
		budgets []int
		need    int
		over    int
		want    bool
	}{
		{"exactly equal", []int{105}, 100, 0, true},
		{"one MiB short", []int{104}, 100, 0, false},
		{"overhead counted", []int{205}, 100, 100, true},
		{"overhead one short", []int{204}, 100, 100, false},
		{"summed over devices", []int{60, 45}, 100, 0, true},
		{"summed one short", []int{60, 44}, 100, 0, false},
		{"fractional margin rounds against", []int{21}, 20, 0, true}, // 21.0 exactly
		{"fractional boundary fits", []int{20}, 19, 0, true},         // 19.95 <= 20
		{"fractional boundary short", []int{19}, 19, 0, false},       // 19.95 > 19
		{"device smaller than overhead contributes nothing", []int{50, 105}, 100, 100, false},
		{"empty", nil, 1, 0, false},
		{"zero need", nil, 0, 0, true},
	}
	for _, c := range cases {
		if got := FitsEstimated(c.budgets, c.need, c.over); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestFitsMeasuredBoundary(t *testing.T) {
	for _, c := range []struct {
		budgets []int
		need    int
		want    bool
	}{
		{[]int{7800}, 7800, true},
		{[]int{7799}, 7800, false},
		{[]int{4000, 3800}, 7800, true},
		{[]int{4000, 3799}, 7800, false},
		{nil, 1, false},
		{nil, 0, true},
	} {
		if got := FitsMeasured(c.budgets, c.need); got != c.want {
			t.Errorf("%v need %d: got %v", c.budgets, c.need, got)
		}
	}
}

// Worked checks from design section 7.2.
func TestWorkedChecks(t *testing.T) {
	omen, alien := 7895, 7629 // 8151-256, 7885-256
	t.Run("gemma OMEN alone fits", func(t *testing.T) {
		if !FitsMeasured([]int{omen}, 7800) {
			t.Fatal("want fit")
		}
	})
	t.Run("gemma Alienware alone does not fit", func(t *testing.T) {
		if FitsMeasured([]int{alien}, 7800) {
			t.Fatal("want no fit")
		}
	})
	t.Run("devstral estimated does not fit both", func(t *testing.T) {
		if FitsEstimated([]int{omen, alien}, 14828, 600) {
			t.Fatal("want no fit")
		}
		// (7895-600)+(7629-600) = 14324 < 15569 (=14828*1.05)
		if FitsEstimated([]int{omen, alien}, 14828, 600) {
			t.Fatal("want no fit")
		}
	})
}
