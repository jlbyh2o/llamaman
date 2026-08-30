package models

import "testing"

// D30 at the column boundary, tested here because this is where the reading
// lives (section 8.3): `models.n_head_kv_json` stores the metadata VERBATIM, so
// it is a scalar on one model and an array on the next, and the calculator
// indexes it per layer either way.

func ptrOf(s string) *string { return &s }

// TestHeadCountKVReadsBothStoredForms is D30 at the column boundary:
// `models.n_head_kv_json` stores the metadata VERBATIM, so it is a scalar on one
// model and an array on the next, and the calculator indexes it per layer either
// way.
func TestHeadCountKVReadsBothStoredForms(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  *string
		want []int
	}{
		{"a scalar is broadcast", ptrOf("2"), []int{2, 2, 2, 2}},
		{"an array is used as written", ptrOf("[4,4,2,1]"), []int{4, 4, 2, 1}},
		{"absent falls back to n_head", nil, []int{8, 8, 8, 8}},
		{"unreadable falls back to n_head", ptrOf("{}"), []int{8, 8, 8, 8}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := headCountKV(tc.raw, 8, 4)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
