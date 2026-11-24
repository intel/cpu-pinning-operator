// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package fuzzgate tests the fuzz gate's failure classifier, which tolerates only the
// fuzzing coordinator's own shutdown at the -fuzztime deadline. Each case is real
// `go test -fuzz` output.
package fuzzgate_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const scriptPath = "../../.github/scripts/fuzz/classify-failure.sh"

const workflowPath = "../../.github/workflows/sec-fuzz.yml"

// coordinatorShutdown is the tolerated failure: healthy execution, the deadline as the only detail.
const coordinatorShutdown = `fuzz: elapsed: 0s, gathering baseline coverage: 0/111 completed
fuzz: elapsed: 3s, gathering baseline coverage: 111/111 completed, now fuzzing with 8 workers
fuzz: elapsed: 30s, execs: 47974 (1763/sec), new interesting: 106 (total: 111)
--- FAIL: FuzzNamespaceMatches (30.12s)
    context deadline exceeded
FAIL
FAIL    github.com/intel/cpu-pinning-operator/internal/match  30.123s
FAIL
`

func classifier(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(scriptPath)
	if err != nil {
		t.Fatalf("resolving %s: %v", scriptPath, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("the sec-fuzz workflow runs %s; it is missing: %v", scriptPath, err)
	}
	// The gate executes it directly, so a lost exec bit breaks it.
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s is not executable (mode %v); the sec-fuzz workflow runs it directly", scriptPath, info.Mode().Perm())
	}
	return abs
}

// classify runs the script over output written to a temp file.
func classify(t *testing.T, target, output string) (verdict, explanation string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "fuzz-output.txt")
	if err := os.WriteFile(file, []byte(output), 0o600); err != nil {
		t.Fatalf("writing fuzz output: %v", err)
	}
	var stderr strings.Builder
	cmd := exec.Command(classifier(t), target, file)
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("classify-failure.sh %s: %v\nstderr: %s", target, err, stderr.String())
	}
	return strings.TrimSpace(string(stdout)), strings.TrimSpace(stderr.String())
}

func TestClassifyFuzzFailure(t *testing.T) {
	const target = "FuzzNamespaceMatches"

	cases := []struct {
		name   string
		target string
		output string
		want   string
	}{
		{
			name:   "coordinator shutdown at the fuzztime boundary",
			target: target,
			output: coordinatorShutdown,
			want:   "deadline",
		},
		{
			// The deadline substring alone must not excuse other failure details.
			name:   "deadline plus a second failure detail",
			target: target,
			output: `--- FAIL: FuzzNamespaceMatches (30.12s)
    nrt_test.go:41: NamespaceMatches("x", "y") = true, want false
    context deadline exceeded
FAIL
`,
			want: "failure",
		},
		{
			name:   "crash on a committed seed",
			target: target,
			output: `--- FAIL: FuzzNamespaceMatches (0.03s)
    --- FAIL: FuzzNamespaceMatches/seed#0 (0.00s)
        match_test.go:88: index out of range
FAIL
`,
			want: "failure",
		},
		{
			name:   "new crasher written",
			target: target,
			output: `fuzz: elapsed: 4s, execs: 1201 (300/sec), new interesting: 3 (total: 5)
--- FAIL: FuzzNamespaceMatches (4.21s)
    Failing input written to testdata/fuzz/FuzzNamespaceMatches/8f2b1c
    To re-run:
    go test -run=FuzzNamespaceMatches/8f2b1c
    context deadline exceeded
FAIL
`,
			want: "failure",
		},
		{
			// The panic lands after the FAIL block, which alone looks like a clean shutdown.
			name:   "panic dumped after the FAIL block",
			target: target,
			output: `--- FAIL: FuzzNamespaceMatches (30.12s)
    context deadline exceeded
panic: runtime error: index out of range [3] with length 2
goroutine 42 [running]:
github.com/intel/cpu-pinning-operator/internal/match.NamespaceMatches(...)
FAIL
`,
			want: "failure",
		},
		{
			name:   "build failure, no FAIL block at all",
			target: target,
			output: `# github.com/intel/cpu-pinning-operator/internal/match [build failed]
internal/match/match_test.go:12:2: undefined: NamespaceMatches
FAIL    github.com/intel/cpu-pinning-operator/internal/match [build failed]
`,
			want: "failure",
		},
		{
			// Another target's deadline does not explain this leg's failure.
			name:   "deadline reported for a different target",
			target: target,
			output: `--- FAIL: FuzzIndexMatch (30.12s)
    context deadline exceeded
FAIL
`,
			want: "failure",
		},
		{
			// FuzzHandleReserve and FuzzHandleReserveIdempotency both exist; a prefix is no match.
			name:   "deadline reported for a target this one is a prefix of",
			target: "FuzzHandleReserve",
			output: `--- FAIL: FuzzHandleReserveIdempotency (30.12s)
    context deadline exceeded
FAIL
`,
			want: "failure",
		},
		{
			name:   "two targets fail",
			target: target,
			output: `--- FAIL: FuzzNamespaceMatches (30.12s)
    context deadline exceeded
--- FAIL: FuzzIndexMatch (30.12s)
    context deadline exceeded
FAIL
`,
			want: "failure",
		},
		{
			name:   "infrastructure failure with no test output",
			target: target,
			output: "failed to connect to the docker API at unix:///run/docker/docker.sock\n",
			want:   "failure",
		},
		{
			name:   "empty output",
			target: target,
			output: "",
			want:   "failure",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, explanation := classify(t, tc.target, tc.output)
			if got != tc.want {
				t.Fatalf("verdict = %q, want %q (explanation: %s)", got, tc.want, explanation)
			}
			if explanation == "" {
				t.Error("no explanation on stderr; the job log has to say why the leg was tolerated or failed")
			}
		})
	}
}

// TestClassifyRejectsBadUsage proves bad arguments exit 2 with no verdict.
func TestClassifyRejectsBadUsage(t *testing.T) {
	script := classifier(t)

	t.Run("missing arguments", func(t *testing.T) {
		assertUsageError(t, exec.Command(script, "FuzzNamespaceMatches"))
	})

	t.Run("output file does not exist", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent.txt")
		assertUsageError(t, exec.Command(script, "FuzzNamespaceMatches", missing))
	})

	// The name reaches a regex, so a pattern must not match another target's FAIL block.
	t.Run("target name is not an identifier", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "fuzz-output.txt")
		if err := os.WriteFile(file, []byte(coordinatorShutdown), 0o600); err != nil {
			t.Fatalf("writing fuzz output: %v", err)
		}
		assertUsageError(t, exec.Command(script, "Fuzz.*", file))
	})
}

// TestWorkflowUsesTheClassifier proves the gate still calls the script, checks its verdict,
// and checks for a new corpus file first.
func TestWorkflowUsesTheClassifier(t *testing.T) {
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("reading %s: %v", workflowPath, err)
	}
	workflow := string(raw)

	for _, want := range []string{
		// The committed script path.
		".github/scripts/fuzz/classify-failure.sh",
		// The verdict printed for a tolerated shutdown.
		"= deadline",
		// A crash cut short by the deadline writes a corpus file and prints nothing.
		"git ls-files --others --exclude-standard",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("%s does not contain %q; the fuzz gate no longer uses the classifier as this package tests it", workflowPath, want)
		}
	}

	if crasherCheck, classify := strings.Index(workflow, "git ls-files --others"), strings.Index(workflow, "classify-failure.sh"); crasherCheck > classify {
		t.Errorf("the workflow consults the classifier (offset %d) before checking for a new corpus file (offset %d); a crash cut short by the fuzztime deadline would be tolerated", classify, crasherCheck)
	}
}

func assertUsageError(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want a non-zero exit; stdout %q", err, out)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2", exitErr.ExitCode())
	}
	if len(strings.TrimSpace(string(out))) != 0 {
		t.Errorf("stdout = %q, want no verdict on a usage error", out)
	}
}
