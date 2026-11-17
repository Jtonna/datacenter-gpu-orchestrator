package core

import "testing"

func TestNodeLiveBudgetMiB(t *testing.T) {
	tests := []struct {
		name string
		node Node
		want int
	}{
		{"omen empty", Node{FreeMiB: 8151, HeadroomMiB: 256}, 7895},
		{"alienware empty", Node{FreeMiB: 7885, HeadroomMiB: 256}, 7629},
		{"below headroom is negative", Node{FreeMiB: 100, HeadroomMiB: 256}, -156},
		{"no headroom", Node{FreeMiB: 500}, 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.node.LiveBudgetMiB(); got != tt.want {
				t.Fatalf("LiveBudgetMiB() = %d, want %d", got, tt.want)
			}
		})
	}
}
