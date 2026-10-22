package orchestrator

import "testing"

// TestParseLabeledContainersKeepsUnlabeledWarmField pins the parsing of
// `docker ps --format '{{.ID}}\t{{.Label "isola.session_id"}}\t{{.Label "isola.warm"}}'`.
//
// A container without an isola.warm label prints an empty third field, so the
// line ends with a tab. Trimming the whole output first (the previous code)
// swallowed that tab on the last line, and a line with two fields was skipped —
// so the sweep always ignored exactly one container, and a lone orphan was
// never reclaimed at all.
func TestParseLabeledContainersKeepsUnlabeledWarmField(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single container without warm label",
			in:   "abc123\tsess-1\t\n",
			want: []string{"abc123"},
		},
		{
			name: "last line keeps its empty warm field",
			in:   "id1\tsess-1\t\nid2\tsess-2\t\n",
			want: []string{"id1", "id2"},
		},
		{
			name: "crlf output",
			in:   "id1\tsess-1\t\r\nid2\tsess-2\t\r\n",
			want: []string{"id1", "id2"},
		},
		{
			name: "warm container is skipped",
			in:   "id1\tsess-1\t1\nid2\tsess-2\t\n",
			want: []string{"id2"},
		},
		{
			name: "empty output",
			in:   "",
			want: nil,
		},
		{
			name: "line without session label",
			in:   "lonely\nid1\tsess-1\t\n",
			want: []string{"id1"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseLabeledContainers(tc.in)
			ids := make([]string, 0, len(got))
			for _, c := range got {
				ids = append(ids, c.ID)
			}
			if len(ids) != len(tc.want) {
				t.Fatalf("got %v, want %v", ids, tc.want)
			}
			for i := range ids {
				if ids[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", ids, tc.want)
				}
			}
		})
	}
}
