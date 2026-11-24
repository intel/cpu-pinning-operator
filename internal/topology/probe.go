// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/intel/cpu-pinning-operator/internal/nrt"
)

// ProbeSubcommandName is the manager subcommand the topology-probe pod runs.
const ProbeSubcommandName = "topology-probe"

// probeRoots are the sysfs/procfs roots the probe reads (a value so tests can fake
// them). It reports physical topology, not the container's cgroup cpuset.
type probeRoots struct {
	node    string
	cpu     string
	cpuInfo string
}

// hostRoots are the paths on the node the probe pod runs on.
var hostRoots = probeRoots{
	node:    "/sys/devices/system/node",
	cpu:     "/sys/devices/system/cpu",
	cpuInfo: "/proc/cpuinfo",
}

// RunProbe writes the node's topology to w for ParseProbeOutput. An unreadable
// kernel file omits its line; only a write failure is an error.
func RunProbe(w io.Writer) error {
	return hostRoots.write(w)
}

func (r probeRoots) write(w io.Writer) error {
	out := bufio.NewWriter(w)

	numaNodes := 0
	for _, nodeDir := range glob(r.node, "node[0-9]*") {
		numaNodes++
		node := strings.TrimPrefix(filepath.Base(nodeDir), "node")
		_, _ = fmt.Fprintf(out, "PKGNODE node=%s pkg=%s\n", node, nodePackageID(nodeDir))

		// Per-NUMA memory in kB.
		if kb := nodeMemTotalKB(nodeDir); kb != "" {
			_, _ = fmt.Fprintf(out, "MEMNODE node=%s kb=%s\n", node, kb)
		}

		// One line per logical CPU with its NUMA id and core id.
		for _, cpuDir := range glob(nodeDir, "cpu[0-9]*") {
			cpu := strings.TrimPrefix(filepath.Base(cpuDir), "cpu")
			_, _ = fmt.Fprintf(out, "CPUINFO cpu=%s numa=%s core=%s\n", cpu, node, r.coreID(cpu))
		}
	}
	// A non-NUMA kernel has no node directory: report one NUMA node, not zero.
	if numaNodes == 0 {
		numaNodes = 1
	}

	_, _ = fmt.Fprintf(out, "NUMA node(s): %d\n", numaNodes)
	_, _ = fmt.Fprintf(out, "CPU(s): %d\n", len(glob(r.cpu, "cpu[0-9]*")))
	_, _ = fmt.Fprintf(out, "Thread(s) per core: %d\n", r.threadsPerCore())
	_, _ = fmt.Fprintf(out, "AMX: %t\n", r.hasAMXTile())

	return out.Flush()
}

// nodePackageID is the physical package the NUMA node sits on, taken from the
// first CPU on the node that reports one. "0" when none does.
func nodePackageID(nodeDir string) string {
	for _, cpuDir := range glob(nodeDir, "cpu[0-9]*") {
		if pkg := readTrimmed(filepath.Join(cpuDir, "topology", "physical_package_id")); pkg != "" {
			return pkg
		}
	}
	return "0"
}

// nodeMemTotalKB is the NUMA node's MemTotal in kB from "Node <N> MemTotal: <kB> kB", or "".
func nodeMemTotalKB(nodeDir string) string {
	f, err := os.Open(filepath.Join(nodeDir, "meminfo")) // #nosec G304 -- path is built from the fixed sysfs node root
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if !strings.Contains(scanner.Text(), "MemTotal") {
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			return ""
		}
		return fields[len(fields)-2]
	}
	return ""
}

// coreID is the lowest CPU id sharing the core (the representative thread); a CPU
// with no sibling list is its own core.
func (r probeRoots) coreID(cpu string) string {
	siblings := nrt.ParseCPUSet(r.threadSiblingsList(cpu))
	if len(siblings) == 0 {
		return cpu
	}
	return strconv.Itoa(siblings[0])
}

// threadsPerCore counts CPU 0's siblings, expanding comma or range lists; default 1.
func (r probeRoots) threadsPerCore() int {
	siblings := nrt.ParseCPUSet(r.threadSiblingsList("0"))
	if len(siblings) == 0 {
		return 1
	}
	return len(siblings)
}

func (r probeRoots) threadSiblingsList(cpu string) string {
	return readTrimmed(filepath.Join(r.cpu, "cpu"+cpu, "topology", "thread_siblings_list"))
}

// hasAMXTile reports the amx_tile flag from /proc/cpuinfo, removing the NFD dependency.
func (r probeRoots) hasAMXTile() bool {
	f, err := os.Open(r.cpuInfo) // #nosec G304 -- the path is a fixed procfs location
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Whole-field match: a substring would accept longer flags.
		for _, field := range strings.Fields(scanner.Text()) {
			if field == "amx_tile" {
				return true
			}
		}
	}
	return false
}

// glob lists paths matching pattern under dir; patterns are literals, so errors are ignored.
func glob(dir, pattern string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return nil
	}
	return matches
}

// readTrimmed is the contents of a one-value kernel file without its trailing
// newline, or "" when the file cannot be read.
func readTrimmed(path string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- path is built from a fixed sysfs root
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
