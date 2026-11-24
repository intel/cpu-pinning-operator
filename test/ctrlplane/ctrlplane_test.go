// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package ctrlplane

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestBinariesPicksNewestCompleteStoreDir proves the store fallback picks the newest
// complete dir for this platform; an empty KUBEBUILDER_ASSETS counts as unset.
func TestBinariesPicksNewestCompleteStoreDir(t *testing.T) {
	both := []string{"kube-apiserver", "etcd"}
	here := "-" + runtime.GOOS + "-" + runtime.GOARCH
	tests := []struct {
		name string
		// store dir name -> binaries installed in it.
		dirs map[string][]string
		want string
	}{
		{
			name: "newest by version, not by name",
			dirs: map[string][]string{"1.9.0" + here: both, "1.37.0" + here: both},
			want: "1.37.0" + here,
		},
		{
			name: "unversioned name never outranks a version",
			dirs: map[string][]string{"latest" + here: both, "1.9.0" + here: both},
			want: "1.9.0" + here,
		},
		{
			name: "another platform never outranks this one",
			dirs: map[string][]string{"1.37.0-nosuchos-nosucharch": both, "1.9.0" + here: both},
			want: "1.9.0" + here,
		},
		{
			name: "half-installed newest is skipped",
			dirs: map[string][]string{
				"1.37.0" + here: {"kube-apiserver"},
				"1.9.0" + here:  both,
			},
			want: "1.9.0" + here,
		},
		{
			name: "no complete dir",
			dirs: map[string][]string{"1.37.0" + here: {"etcd"}},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), "kubebuilder-envtest", "k8s")
			for dir, bins := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(store, dir), 0o755); err != nil {
					t.Fatal(err)
				}
				for _, bin := range bins {
					if err := os.WriteFile(filepath.Join(store, dir, bin), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			t.Setenv("KUBEBUILDER_ASSETS", "")
			t.Setenv("XDG_DATA_HOME", filepath.Dir(filepath.Dir(store)))

			assets, ok := binaries()
			want := ""
			if tc.want != "" {
				want = filepath.Join(store, tc.want)
			}
			if assets != want || ok != (tc.want != "") {
				t.Errorf("binaries() = %q, %v; want %q, %v", assets, ok, want, tc.want != "")
			}
		})
	}
}
