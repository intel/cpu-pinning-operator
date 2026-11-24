// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeKernelFile writes one value file into a fabricated sysfs/procfs tree.
func writeKernelFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// rootsUnder addresses a fabricated tree the way hostRoots addresses the node.
func rootsUnder(root string) probeRoots {
	return probeRoots{
		node:    filepath.Join(root, "sys/devices/system/node"),
		cpu:     filepath.Join(root, "sys/devices/system/cpu"),
		cpuInfo: filepath.Join(root, "proc/cpuinfo"),
	}
}

// fabricateTree builds a 2-NUMA, 8-CPU SMT2 machine (CPU i and i+4 share a core); amx sets amx_tile.
func fabricateTree(t *testing.T, amx bool) string {
	t.Helper()
	root := t.TempDir()
	nodeCPUs := map[int][]int{0: {0, 1, 4, 5}, 1: {2, 3, 6, 7}}
	memKB := map[int]string{0: "1048576", 1: "2097152"}

	for node, cpus := range nodeCPUs {
		nodeDir := filepath.Join(root, "sys/devices/system/node", "node"+strconv.Itoa(node))
		writeKernelFile(t, filepath.Join(nodeDir, "meminfo"),
			"Node "+strconv.Itoa(node)+" MemTotal:       "+memKB[node]+" kB\nNode "+strconv.Itoa(node)+" MemFree:        16 kB\n")
		for _, cpu := range cpus {
			// Both the per-node view and the global one, as the kernel exposes them.
			writeKernelFile(t, filepath.Join(nodeDir, "cpu"+strconv.Itoa(cpu), "topology", "physical_package_id"), strconv.Itoa(node)+"\n")
			writeKernelFile(t, filepath.Join(root, "sys/devices/system/cpu", "cpu"+strconv.Itoa(cpu), "topology", "thread_siblings_list"),
				strconv.Itoa(cpu%4)+","+strconv.Itoa(cpu%4+4)+"\n")
		}
	}

	flags := "fpu vme de pse tsc avx512f"
	if amx {
		flags += " amx_bf16 amx_tile amx_int8"
	}
	var cpuinfo strings.Builder
	for cpu := 0; cpu < 8; cpu++ {
		cpuinfo.WriteString("processor\t: " + strconv.Itoa(cpu) + "\nflags\t\t: " + flags + "\n\n")
	}
	writeKernelFile(t, filepath.Join(root, "proc/cpuinfo"), cpuinfo.String())
	return root
}

func runProbe(t *testing.T, root string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := rootsUnder(root).write(&buf); err != nil {
		t.Fatalf("writing the probe output: %v", err)
	}
	return buf.String()
}

// TestRunProbeReadsTheHost runs the probe on the host and asserts only the summary lines.
func TestRunProbeReadsTheHost(t *testing.T) {
	var buf bytes.Buffer
	if err := RunProbe(&buf); err != nil {
		t.Fatalf("RunProbe: %v", err)
	}
	for _, want := range []string{"NUMA node(s): ", "CPU(s): ", "Thread(s) per core: ", "AMX: "} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("probe output has no %q line:\n%s", want, buf.String())
		}
	}
}

// TestRunProbeOutput pins the probe's output byte for byte, since the parser depends on it.
func TestRunProbeOutput(t *testing.T) {
	root := fabricateTree(t, true)

	const want = `PKGNODE node=0 pkg=0
MEMNODE node=0 kb=1048576
CPUINFO cpu=0 numa=0 core=0
CPUINFO cpu=1 numa=0 core=1
CPUINFO cpu=4 numa=0 core=0
CPUINFO cpu=5 numa=0 core=1
PKGNODE node=1 pkg=1
MEMNODE node=1 kb=2097152
CPUINFO cpu=2 numa=1 core=2
CPUINFO cpu=3 numa=1 core=3
CPUINFO cpu=6 numa=1 core=2
CPUINFO cpu=7 numa=1 core=3
NUMA node(s): 2
CPU(s): 8
Thread(s) per core: 2
AMX: true
`
	if got := runProbe(t, root); got != want {
		t.Errorf("probe output:\n%s\nwant:\n%s", got, want)
	}
}

// TestRunProbeOutputParses proves the probe's output parses back to the machine it described.
func TestRunProbeOutputParses(t *testing.T) {
	root := fabricateTree(t, true)

	topo, err := ParseProbeOutput(runProbe(t, root))
	if err != nil {
		t.Fatalf("ParseProbeOutput on the probe's own output: %v", err)
	}
	if topo.NumaNodes != 2 {
		t.Errorf("NumaNodes = %d, want 2", topo.NumaNodes)
	}
	if topo.LogicalCPUs != 8 {
		t.Errorf("LogicalCPUs = %d, want 8", topo.LogicalCPUs)
	}
	if topo.ThreadsPerCore != 2 {
		t.Errorf("ThreadsPerCore = %d, want 2", topo.ThreadsPerCore)
	}
	if topo.PhysicalCPUs != 4 {
		t.Errorf("PhysicalCPUs = %d, want 4", topo.PhysicalCPUs)
	}
	if topo.MaxSpan != 2 {
		t.Errorf("MaxSpan = %d, want 2", topo.MaxSpan)
	}
	if !topo.AMX {
		t.Error("AMX = false, want true (amx_tile is in the fabricated cpuinfo)")
	}
	// The floor is the SMALLEST per-NUMA total, which is NUMA 0's.
	if want := int64(1048576 * 1024); topo.MemoryPerNUMABytes != want {
		t.Errorf("MemoryPerNUMABytes = %d, want %d", topo.MemoryPerNUMABytes, want)
	}
	if len(topo.CPUs) != 8 {
		t.Errorf("CPUs has %d entries, want 8", len(topo.CPUs))
	}
	// Four physical cores, each claimed by the lower of its two siblings.
	for cpu, wantCore := range map[int]int{0: 0, 4: 0, 1: 1, 5: 1, 2: 2, 6: 2, 3: 3, 7: 3} {
		if got := topo.CPUs[cpu].Core; got != wantCore {
			t.Errorf("CPU %d core = %d, want %d", cpu, got, wantCore)
		}
	}
	if len(topo.PkgNode) != 2 {
		t.Errorf("PkgNode = %v, want one NUMA node on each of two packages", topo.PkgNode)
	}
}

// TestRunProbeThreadsPerCore covers comma and range sibling-list forms.
func TestRunProbeThreadsPerCore(t *testing.T) {
	for name, tc := range map[string]struct {
		siblings string
		want     string
	}{
		"comma form, SMT2":     {"0,64", "Thread(s) per core: 2"},
		"range form, SMT2":     {"0-1", "Thread(s) per core: 2"},
		"single-threaded core": {"0", "Thread(s) per core: 1"},
		"four-way SMT":         {"0-3", "Thread(s) per core: 4"},
		"mixed forms":          {"0-1,64-65", "Thread(s) per core: 4"},
		"empty list":           {"", "Thread(s) per core: 1"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeKernelFile(t, filepath.Join(root, "sys/devices/system/cpu/cpu0/topology/thread_siblings_list"), tc.siblings+"\n")

			if got := runProbe(t, root); !strings.Contains(got, tc.want+"\n") {
				t.Errorf("probe output has no %q for thread_siblings_list %q:\n%s", tc.want, tc.siblings, got)
			}
		})
	}
}

// TestRunProbeMissingKernelFiles proves an unreadable file omits its line without failing the probe.
func TestRunProbeMissingKernelFiles(t *testing.T) {
	t.Run("no per-NUMA meminfo omits MEMNODE", func(t *testing.T) {
		root := fabricateTree(t, false)
		for _, node := range []string{"node0", "node1"} {
			if err := os.Remove(filepath.Join(root, "sys/devices/system/node", node, "meminfo")); err != nil {
				t.Fatal(err)
			}
		}
		out := runProbe(t, root)
		if strings.Contains(out, "MEMNODE") {
			t.Errorf("probe emitted a MEMNODE line with no meminfo to read:\n%s", out)
		}
		topo, err := ParseProbeOutput(out)
		if err != nil {
			t.Fatalf("ParseProbeOutput: %v", err)
		}
		// Zero is how the parser says "unknown", which skips the memory capacity check.
		if topo.MemoryPerNUMABytes != 0 || topo.MemoryPerNUMABytesMap != nil {
			t.Errorf("per-NUMA memory = %d / %v, want unknown", topo.MemoryPerNUMABytes, topo.MemoryPerNUMABytesMap)
		}
	})

	t.Run("no cpuinfo reports no AMX", func(t *testing.T) {
		root := fabricateTree(t, true)
		if err := os.Remove(filepath.Join(root, "proc/cpuinfo")); err != nil {
			t.Fatal(err)
		}
		if got := runProbe(t, root); !strings.Contains(got, "AMX: false\n") {
			t.Errorf("probe output has no \"AMX: false\":\n%s", got)
		}
	})

	t.Run("no NUMA directory reports one node", func(t *testing.T) {
		root := t.TempDir()
		writeKernelFile(t, filepath.Join(root, "sys/devices/system/cpu/cpu0/topology/thread_siblings_list"), "0,1\n")
		writeKernelFile(t, filepath.Join(root, "sys/devices/system/cpu/cpu1/topology/thread_siblings_list"), "0,1\n")

		out := runProbe(t, root)
		topo, err := ParseProbeOutput(out)
		if err != nil {
			t.Fatalf("ParseProbeOutput on a kernel with no NUMA sysfs: %v\n%s", err, out)
		}
		if topo.NumaNodes != 1 || topo.LogicalCPUs != 2 {
			t.Errorf("topology = %d NUMA / %d CPUs, want 1 / 2", topo.NumaNodes, topo.LogicalCPUs)
		}
	})

	t.Run("nothing readable is rejected, not reported as a machine", func(t *testing.T) {
		out := runProbe(t, t.TempDir())
		if _, err := ParseProbeOutput(out); err == nil {
			t.Errorf("an unreadable sysfs parsed as a topology:\n%s", out)
		}
	})
}
