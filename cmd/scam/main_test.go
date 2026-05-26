package main

import "testing"

func TestResolveClusterID(t *testing.T) {
	cases := []struct {
		name      string
		envID     string
		kubeUID   string
		want      string
		wantThunk bool // expect the kube-system thunk to be invoked
	}{
		{
			name:      "CLUSTER_ID env wins and short-circuits the thunk",
			envID:     "override-123",
			kubeUID:   "kube-uid-should-not-appear",
			want:      "override-123",
			wantThunk: false,
		},
		{
			name:      "CLUSTER_ID empty falls through to kube-system UID",
			envID:     "",
			kubeUID:   "kube-uid-abc",
			want:      "kube-uid-abc",
			wantThunk: true,
		},
		{
			name:      "CLUSTER_ID whitespace is treated as empty",
			envID:     "   ",
			kubeUID:   "kube-uid-xyz",
			want:      "kube-uid-xyz",
			wantThunk: true,
		},
		{
			name:      "no override and no kube-system UID yields empty",
			envID:     "",
			kubeUID:   "",
			want:      "",
			wantThunk: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLUSTER_ID", tc.envID)

			called := false
			thunk := func() string {
				called = true
				return tc.kubeUID
			}

			got := resolveClusterID(thunk)
			if got != tc.want {
				t.Errorf("resolveClusterID = %q, want %q", got, tc.want)
			}
			if called != tc.wantThunk {
				t.Errorf("thunk called = %v, want %v", called, tc.wantThunk)
			}
		})
	}
}

func TestResolveClusterName(t *testing.T) {
	t.Run("ROR name wins over env var", func(t *testing.T) {
		t.Setenv("CLUSTER_NAME", "from-env")
		if got := resolveClusterName("from-ror"); got != "from-ror" {
			t.Errorf("got %q, want %q", got, "from-ror")
		}
	})
	t.Run("empty ROR name falls back to env var", func(t *testing.T) {
		t.Setenv("CLUSTER_NAME", "from-env")
		if got := resolveClusterName(""); got != "from-env" {
			t.Errorf("got %q, want %q", got, "from-env")
		}
	})
}

func TestResolveEnvironment(t *testing.T) {
	t.Run("ROR env wins over env var", func(t *testing.T) {
		t.Setenv("ENVIRONMENT", "from-env")
		if got := resolveEnvironment("from-ror"); got != "from-ror" {
			t.Errorf("got %q, want %q", got, "from-ror")
		}
	})
	t.Run("empty ROR env falls back to env var", func(t *testing.T) {
		t.Setenv("ENVIRONMENT", "from-env")
		if got := resolveEnvironment(""); got != "from-env" {
			t.Errorf("got %q, want %q", got, "from-env")
		}
	})
}
