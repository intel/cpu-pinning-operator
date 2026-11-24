// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

// Package nrt parses NodeResourceTopology CRs. Metrics and the webhook share
// this occupancy definition. Leaf package: do not import controller or webhook.
package nrt

import (
	"sort"
	"strconv"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

const (
	containerZoneType = "allocation for container"
	balloonZoneType   = "balloon"

	reservedBalloonName = "reserved"

	cpusetAttribute = "cpuset"
	// memorySetAttribute is "memory set" — not symmetric with "cpuset". Empty
	// means the balloon does not pin memory, not an error.
	memorySetAttribute = "memory set"
)

// GVK is the NodeResourceTopology CR. Unstructured so the module does not pull the typed API.
var GVK = schema.GroupVersionKind{Group: "topology.node.k8s.io", Version: "v1alpha2", Kind: "NodeResourceTopology"}

// zonesOf: ok is false when "zones" has the wrong type. Treating that as empty
// would read a corrupt CR as fully free. An absent field is (nil, true).
func zonesOf(nrt *unstructured.Unstructured) (zones []interface{}, ok bool) {
	zones, _, err := unstructured.NestedSlice(nrt.Object, "zones")
	if err != nil {
		klog.ErrorS(err, "malformed NodeResourceTopology: \"zones\" is of an unexpected type; treating node topology as unknown", "node", nrt.GetName())
		return nil, false
	}
	return zones, true
}

// Zones is zonesOf for callers that walk zones themselves. Same ok signal.
func Zones(nrt *unstructured.Unstructured) (zones []interface{}, ok bool) {
	return zonesOf(nrt)
}

// nestedString reads a string field; absent or malformed reads as "", never an error.
func nestedString(m map[string]interface{}, fields ...string) string {
	v, found, err := unstructured.NestedString(m, fields...)
	if err != nil || !found {
		return ""
	}
	return v
}

// nestedSlice reads a slice field; absent or malformed reads as nil (unlike zonesOf).
func nestedSlice(m map[string]interface{}, fields ...string) []interface{} {
	s, found, err := unstructured.NestedSlice(m, fields...)
	if err != nil || !found {
		return nil
	}
	return s
}

// OccupiedCPUs is the reserved balloon cpuset union every workload container
// subzone. Reserved container subzones and workload balloon parents are skipped
// so they are not double-counted. ok false means unknown, not empty.
func OccupiedCPUs(nrt *unstructured.Unstructured) (cpus []int, ok bool) {
	zones, ok := zonesOf(nrt)
	if !ok {
		return nil, false
	}
	occupied := map[int]bool{}
	for _, z := range zones {
		zone, ok := z.(map[string]interface{})
		if !ok {
			continue
		}
		zoneType := nestedString(zone, "type")
		name := nestedString(zone, "name")
		cpus := ParseCPUSet(ZoneCpuset(zone))
		if len(cpus) == 0 {
			continue
		}
		switch zoneType {
		case containerZoneType:
			if StripInstance(ZoneParent(zone)) == reservedBalloonName {
				continue
			}
			for _, c := range cpus {
				occupied[c] = true
			}
		case balloonZoneType:
			if StripInstance(name) == reservedBalloonName {
				for _, c := range cpus {
					occupied[c] = true
				}
			}
		}
	}
	out := make([]int, 0, len(occupied))
	for c := range occupied {
		out = append(out, c)
	}
	sort.Ints(out)
	return out, true
}

// HasZones is false when zones are missing or malformed. That is unknown, not unpinned.
func HasZones(nrt *unstructured.Unstructured) bool {
	zones, ok := zonesOf(nrt)
	return ok && len(zones) > 0
}

// ContainerBalloon is the parent balloon of a container zone, instance suffix stripped.
// found is false when the zone is missing, has an empty cpuset, or names no parent —
// reserved-pool means unpinned, and an empty parent must not look like a workload balloon.
func ContainerBalloon(nrt *unstructured.Unstructured, zoneName string) (balloon string, found bool) {
	zones, ok := zonesOf(nrt)
	if !ok {
		return "", false // malformed CR → unknown, not a claim the container is unpinned
	}
	for _, z := range zones {
		zone, ok := z.(map[string]interface{})
		if !ok {
			continue
		}
		zoneType := nestedString(zone, "type")
		if zoneType != containerZoneType {
			continue
		}
		if name := nestedString(zone, "name"); name != zoneName {
			continue
		}
		if len(ParseCPUSet(ZoneCpuset(zone))) == 0 {
			return "", false
		}

		parent := StripInstance(ZoneParent(zone))
		if parent == "" {
			return "", false
		}
		return parent, true
	}
	return "", false
}

// ZoneCpuset returns the value of a zone's "cpuset" attribute, or "".
func ZoneCpuset(zone map[string]interface{}) string {
	return zoneAttribute(zone, cpusetAttribute)
}

// ZoneMemorySet returns a zone's pinned NUMA-node-id range, or "" when it pins no memory.
func ZoneMemorySet(zone map[string]interface{}) string {
	return zoneAttribute(zone, memorySetAttribute)
}

func zoneAttribute(zone map[string]interface{}, name string) string {
	for _, a := range nestedSlice(zone, "attributes") {
		attr, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		if n := nestedString(attr, "name"); n == name {
			return nestedString(attr, "value")
		}
	}
	return ""
}

func zoneName(zone map[string]interface{}) string {
	return nestedString(zone, "name")
}

// ZoneType returns a zone's "type", or "".
func ZoneType(zone map[string]interface{}) string {
	return nestedString(zone, "type")
}

// ZoneName returns a zone's "name", or "".
func ZoneName(zone map[string]interface{}) string {
	return zoneName(zone)
}

// ZoneParent returns a zone's parent zone name (the owning balloon), or "".
func ZoneParent(zone map[string]interface{}) string {
	return nestedString(zone, "parent")
}

// PinnedMemoryContainer is one container's memory-pin NUMAs; NRT carries no byte size.
type PinnedMemoryContainer struct {
	Zone string

	NUMAs []int
}

// PinnedMemoryContainers lists workload containers that pin memory. ok false is unknown, not "no pinned memory".
func PinnedMemoryContainers(nrt *unstructured.Unstructured) (containers []PinnedMemoryContainer, ok bool) {
	zones, ok := zonesOf(nrt)
	if !ok {
		return nil, false
	}
	var out []PinnedMemoryContainer
	for _, z := range zones {
		zone, ok := z.(map[string]interface{})
		if !ok {
			continue
		}
		zoneType := nestedString(zone, "type")
		if zoneType != containerZoneType {
			continue
		}
		if StripInstance(ZoneParent(zone)) == reservedBalloonName {
			continue
		}
		numas := ParseCPUSet(ZoneMemorySet(zone))
		if len(numas) == 0 {
			continue
		}
		out = append(out, PinnedMemoryContainer{Zone: zoneName(zone), NUMAs: numas})
	}
	return out, true
}

// ContainerZoneNames keeps duplicates: multiple zones for one container are the
// ghost-allocation signature after CreateContainer without RemoveContainer.
func ContainerZoneNames(nrt *unstructured.Unstructured) (names []string, ok bool) {
	zones, ok := zonesOf(nrt)
	if !ok {
		return nil, false
	}
	var out []string
	for _, z := range zones {
		zone, ok := z.(map[string]interface{})
		if !ok {
			continue
		}
		zoneType := nestedString(zone, "type")
		if zoneType != containerZoneType {
			continue
		}
		if StripInstance(ZoneParent(zone)) == reservedBalloonName {
			continue
		}
		if len(ParseCPUSet(ZoneCpuset(zone))) == 0 {
			continue
		}
		if name := zoneName(zone); name != "" {
			out = append(out, name)
		}
	}
	return out, true
}

// SplitContainerZoneName parses "<namespace>/<pod>/<container>"; missing fields are "".
func SplitContainerZoneName(name string) (namespace, pod, container string) {
	parts := strings.SplitN(name, "/", 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], parts[1], ""
	default:
		return "", "", ""
	}
}

// StripInstance removes the NRI "[N]" instance suffix.
func StripInstance(name string) string {
	if i := strings.IndexByte(name, '['); i >= 0 {
		return name[:i]
	}
	return name
}

// maxCPUID is the kernel CONFIG_NR_CPUS ceiling on x86_64. Duplicated from
// internal/topology because both are leaf packages — keep the two in step.
const maxCPUID = 8191

func cpuSetRanges(s string, fn func(l, h int)) {
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			l, err1 := strconv.Atoi(strings.TrimSpace(lo))
			h, err2 := strconv.Atoi(strings.TrimSpace(hi))
			// Safety bound: ParseCPUSet indexes depth[h+1] unchecked.
			if err1 != nil || err2 != nil || l > h || l < 0 || h > maxCPUID {
				continue
			}
			fn(l, h)
			continue
		}
		if c, err := strconv.Atoi(part); err == nil && c >= 0 && c <= maxCPUID {
			fn(c, c)
		}
	}
}

// ParseCPUSet never drops a CPU to save work: a missed id reads as free and a
// pod lands on a running workload. Ranges mark endpoints only so a 1 MiB
// attribute cannot drive billions of writes on the admission path.
func ParseCPUSet(s string) []int {
	maxID := -1
	cpuSetRanges(s, func(_, h int) {
		if h > maxID {
			maxID = h
		}
	})
	if maxID < 0 {
		return nil
	}
	buf := cpuSetDepthPool.Get().(*[]int32)
	defer cpuSetDepthPool.Put(buf)
	depth := (*buf)[:maxID+2]
	for i := range depth {
		depth[i] = 0 // the pooled buffer may carry a previous call's counts
	}
	cpuSetRanges(s, func(l, h int) { depth[l]++; depth[h+1]-- })

	var out []int
	var open int32
	for id := 0; id <= maxID; id++ {
		if open += depth[id]; open > 0 {
			out = append(out, id)
		}
	}
	return out
}

// cpuSetDepthPool reuses the difference-array buffer on the admission path. Its
// maxCPUID+2 length relies on the h > maxCPUID guard in cpuSetRanges; loosen that
// and an untrusted NRT cpuset panics the webhook.
var cpuSetDepthPool = sync.Pool{New: func() any {
	b := make([]int32, maxCPUID+2)
	return &b
}}
