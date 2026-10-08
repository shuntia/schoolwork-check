package canvas

import "testing"

func TestParseLinkHeader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			name: "canvas style",
			in:   `<https://x.instructure.com/api/v1/planner/items?page=2&per_page=100>; rel="next",<https://x.instructure.com/api/v1/planner/items?page=1&per_page=100>; rel="current",<https://x.instructure.com/api/v1/planner/items?page=5&per_page=100>; rel="last"`,
			want: map[string]string{
				"next":    "https://x.instructure.com/api/v1/planner/items?page=2&per_page=100",
				"current": "https://x.instructure.com/api/v1/planner/items?page=1&per_page=100",
				"last":    "https://x.instructure.com/api/v1/planner/items?page=5&per_page=100",
			},
		},
		{
			name: "url containing a comma and semicolon",
			in:   `<https://x.test/api/v1/items?filter=a,b;c&page=2>; rel="next", <https://x.test/api/v1/items?page=1>; rel="prev"`,
			want: map[string]string{
				"next": "https://x.test/api/v1/items?filter=a,b;c&page=2",
				"prev": "https://x.test/api/v1/items?page=1",
			},
		},
		{
			name: "extra params and unquoted rel",
			in:   `<https://x.test/p2>; type="application/json"; rel=next`,
			want: map[string]string{"next": "https://x.test/p2"},
		},
		{
			name: "multiple rel values on one link",
			in:   `<https://x.test/p9>; rel="last next"`,
			want: map[string]string{"last": "https://x.test/p9", "next": "https://x.test/p9"},
		},
		{name: "empty", in: "", want: map[string]string{}},
		{name: "garbage", in: "not a link header", want: map[string]string{}},
		{name: "unterminated", in: "<https://x.test/p1", want: map[string]string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLinkHeader(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("rel %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
