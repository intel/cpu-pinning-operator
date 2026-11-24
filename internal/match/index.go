// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package match

import (
	"crypto/sha256"
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/classification"
)

// IntentRule is a compiled per-source extractor. CompileIntent raises unset
// Default to 1 so span never collapses to 0.
type IntentRule struct {
	Annotation string
	ArgRegex   *regexp.Regexp
	EnvVar     string
	Default    int
}

// IntentRules is one PolicyRule's compiled extractors. Multiplier nil means
// no multiplier (effective 1), not a zero source.
type IntentRules struct {
	Span       IntentRule
	Multiplier *IntentRule
	CoLocate   string // "" | "package" | "numa"
}

// PolicyRule is one BalloonPinningPolicy's per-workload knobs. Node-global
// settings live on ClusterConfig, not here.
type PolicyRule struct {
	// Name attributes a match back to its policy; it is not used for matching.
	Name string
	// Namespaces empty matches nothing.
	Namespaces      []string
	ByContainerName []string
	ByPodLabels     map[string]string

	ContainerSelection string
	// EnforcePinning is a REQUIRED nodeAffinity on the balloons-enabled label.
	EnforcePinning bool

	NodeFeatures map[string]cpopv1alpha1.NodeFeatureMode

	Intent IntentRules

	CapacityCheckMode string

	DriftPolicy string
}

// ClusterConfig is the singleton BalloonsClusterConfig compiled for the webhook.
// CPUAllocation/Reserved are cluster defaults, layered per node by NodeOverrides.
// ExcludeNode is cluster-global — it is not overridable per node.
type ClusterConfig struct {
	// CPUAllocation "" means no config present yet.
	CPUAllocation string
	// Packing "" means no config; callers default to "spread".
	Packing string
	// HTSiblingPolicy is unused in hyperthreads mode and not per-node overridable; "" means exclude.
	HTSiblingPolicy string
	// ReservedNamespaces: nil → callers apply "*" (node-exhaustion hazard on
	// shared/OpenShift nodes); empty slice → route nothing extra.
	ReservedNamespaces []string
	// Reserved all-nil falls back to 12 logical CPUs.
	Reserved *cpopv1alpha1.ReservedSizingSpec
	// NodeOverrides: first matching selector wins; non-empty fields layer per-field.
	NodeOverrides []NodeConfigOverride

	ExcludeNode labels.Selector
	// Classification zero value runs auto-classification.
	Classification classification.Spec
	// ClassProfiles sit above the cluster default and beneath NodeOverrides.
	ClassProfiles map[string]ClassProfile
}

// ClassProfile is a class's balloon base. Empty fields inherit the cluster default.
type ClassProfile struct {
	CPUAllocation string
	Reserved      *cpopv1alpha1.ReservedSizingSpec
}

// NodeConfigOverride is one compiled override. Empty CPUAllocation / nil Reserved inherit.
type NodeConfigOverride struct {
	Selector      labels.Selector
	CPUAllocation string
	Reserved      *cpopv1alpha1.ReservedSizingSpec
}

// Index is the live match-rule set plus cluster config. Node topology is not held
// here: the webhook reads Node labels so a restart cannot admit pods unpinned.
type Index struct {
	mu      sync.RWMutex
	rules   []PolicyRule
	cluster ClusterConfig
}

// NewIndex creates an empty match Index.
func NewIndex() *Index {
	return &Index{}
}

// Update replaces the current set of match rules. Match tries them in name
// order, so on an overlap the policy whose name sorts first wins.
func (idx *Index) Update(rules []PolicyRule) {
	// List order is a map walk; name order makes overlaps resolve the same on
	// every replica and in every caller of Match.
	rules = slices.Clone(rules)
	slices.SortStableFunc(rules, func(a, b PolicyRule) int { return strings.Compare(a.Name, b.Name) })
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.rules = rules
}

// SetClusterConfig replaces the node-global cluster config. An absent singleton clears it to zero.
func (idx *Index) SetClusterConfig(cfg ClusterConfig) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	// slices.Clone preserves nil vs empty: callers treat nil as "not configured" (*)
	// and empty as "route nothing extra".
	cfg.ReservedNamespaces = slices.Clone(cfg.ReservedNamespaces)
	idx.cluster = cfg
}

// NodeExcluded reports whether the cluster exclusion selector matches nodeLabels.
func (idx *Index) NodeExcluded(nodeLabels map[string]string) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.cluster.ExcludeNode == nil {
		return false
	}
	return idx.cluster.ExcludeNode.Matches(labels.Set(nodeLabels))
}

// CPUAllocation is the cluster default; per-node decisions prefer the node's published mode.
func (idx *Index) CPUAllocation() string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.cluster.CPUAllocation
}

// Packing returns "" when no BalloonsClusterConfig exists; callers default to "spread".
func (idx *Index) Packing() string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.cluster.Packing
}

// HTSiblingPolicy returns "" when no BalloonsClusterConfig exists; callers default to "exclude".
func (idx *Index) HTSiblingPolicy() string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.cluster.HTSiblingPolicy
}

// ReservedNamespaces returns nil when unset (callers apply "*") and empty for "route nothing extra".
func (idx *Index) ReservedNamespaces() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	// Clone so callers cannot mutate the index after the lock is released.
	return slices.Clone(idx.cluster.ReservedNamespaces)
}

// ReservedSizing is the cluster default. Per-node overrides go through ResolveNodeConfig.
func (idx *Index) ReservedSizing() *cpopv1alpha1.ReservedSizingSpec {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.cluster.Reserved
}

// ResolveNodeConfig is ResolveNodeConfigForClass with no class profile applied.
func (idx *Index) ResolveNodeConfig(nodeLabels map[string]string) (cpuAllocation string, reserved *cpopv1alpha1.ReservedSizingSpec) {
	return idx.ResolveNodeConfigForClass(nodeLabels, "")
}

// Classification zero value (no BalloonsClusterConfig yet) runs auto-classification.
func (idx *Index) Classification() classification.Spec {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.cluster.Classification
}

// ResolveNodeConfigForClass layers cluster default → class profile → first matching override, per field.
func (idx *Index) ResolveNodeConfigForClass(nodeLabels map[string]string, class string) (cpuAllocation string, reserved *cpopv1alpha1.ReservedSizingSpec) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	cpuAllocation = idx.cluster.CPUAllocation
	reserved = idx.cluster.Reserved
	if class != "" {
		if profile, ok := idx.cluster.ClassProfiles[class]; ok {
			if profile.CPUAllocation != "" {
				cpuAllocation = profile.CPUAllocation
			}
			if profile.Reserved != nil {
				reserved = profile.Reserved
			}
		}
	}
	set := labels.Set(nodeLabels)
	for i := range idx.cluster.NodeOverrides {
		ov := &idx.cluster.NodeOverrides[i]
		if ov.Selector == nil || !ov.Selector.Matches(set) {
			continue
		}
		// First match wins; only the fields it sets are applied.
		if ov.CPUAllocation != "" {
			cpuAllocation = ov.CPUAllocation
		}
		if ov.Reserved != nil {
			reserved = ov.Reserved
		}
		break
	}
	return cpuAllocation, reserved
}

// MatchResult is the outcome of matching a pod. When Rule != nil, exactly one of
// Container, PodScoped, or Ambiguous is set. Ambiguous means the pod is a target
// but cannot be pinned without picking a container.
type MatchResult struct {
	Rule      *PolicyRule
	Container *corev1.Container
	PodScoped bool
	Ambiguous bool
	Reason    string
}

// Match returns the match outcome for a pod; Rule is nil for a non-target. Under
// wholePod (default) a byContainerName match hitting multiple containers is
// Ambiguous; largestContainer picks the highest requests.cpu.
func (idx *Index) Match(pod *corev1.Pod) MatchResult {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	for i := range idx.rules {
		rule := &idx.rules[i]
		// Otherwise a rule for A matches B whenever another policy opened B.
		if !ruleMatchesNamespace(rule, pod.Namespace) {
			continue
		}

		if candidates := matchByContainerName(pod, rule.ByContainerName); len(candidates) > 0 {
			if len(candidates) == 1 {
				return MatchResult{Rule: rule, Container: candidates[0]}
			}
			if rule.ContainerSelection == containerSelectionLargest {
				return MatchResult{Rule: rule, Container: largestContainer(candidates)}
			}
			return MatchResult{
				Rule:      rule,
				Ambiguous: true,
				Reason: fmt.Sprintf("byContainerName matched %d containers; set spec.match.containerSelection=largestContainer to disambiguate",
					len(candidates)),
			}
		}

		if matchByPodLabels(pod, rule.ByPodLabels) {
			if rule.ContainerSelection == containerSelectionLargest {
				if c := largestContainer(containerPtrs(pod)); c != nil {
					return MatchResult{Rule: rule, Container: c}
				}
			}
			return MatchResult{Rule: rule, PodScoped: true}
		}
	}
	return MatchResult{}
}

const (
	containerSelectionWholePod = "wholePod"
	containerSelectionLargest  = "largestContainer"
)

// largestContainer picks the highest requests.cpu; ties keep pod-spec order
// (candidates must already be in that order).
func largestContainer(candidates []*corev1.Container) *corev1.Container {
	var best *corev1.Container
	for _, c := range candidates {
		if best == nil {
			best = c
			continue
		}
		// Cmp, not MilliValue: MilliValue overflows on an absurd request.
		req := c.Resources.Requests[corev1.ResourceCPU]
		bestReq := best.Resources.Requests[corev1.ResourceCPU]
		if req.Cmp(bestReq) > 0 {
			best = c
		}
	}
	return best
}

func containerPtrs(pod *corev1.Pod) []*corev1.Container {
	out := make([]*corev1.Container, 0, len(pod.Spec.Containers))
	for i := range pod.Spec.Containers {
		out = append(out, &pod.Spec.Containers[i])
	}
	return out
}

func ruleMatchesNamespace(rule *PolicyRule, ns string) bool {
	for _, pattern := range rule.Namespaces {
		if NamespaceMatches(pattern, ns) {
			return true
		}
	}
	return false
}

// NamespaceMatches treats '*'/'?' as a shell glob, otherwise exact. Shared with
// webhook-config expansion so both surfaces agree.
func NamespaceMatches(pattern, ns string) bool {
	if !IsGlob(pattern) {
		return pattern == ns
	}
	ok, err := path.Match(pattern, ns)
	return err == nil && ok
}

// IsGlob reports whether a pattern contains shell-glob metacharacters.
func IsGlob(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

func matchByContainerName(pod *corev1.Pod, patterns []string) []*corev1.Container {
	if len(patterns) == 0 {
		return nil
	}
	var out []*corev1.Container
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		for _, pattern := range patterns {
			if containerNameMatches(pattern, c.Name) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func containerNameMatches(pattern, name string) bool {
	if !IsGlob(pattern) {
		return pattern == name
	}
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func matchByPodLabels(pod *corev1.Pod, labels map[string]string) bool {
	if len(labels) == 0 {
		return false
	}
	for k, v := range labels {
		if podVal, ok := pod.Labels[k]; ok && podVal == v {
			return true
		}
	}
	return false
}

// CompileIntent pre-compiles argRegex. A bad regex is an error so the reconciler
// surfaces it instead of silently disabling the source at every admission.
func CompileIntent(spec cpopv1alpha1.IntentSpec) (IntentRules, error) {
	span, err := compileIntentSource(spec.Span, "span")
	if err != nil {
		return IntentRules{}, err
	}
	rules := IntentRules{Span: span, CoLocate: spec.CoLocate}
	if spec.Multiplier != nil {
		mult, err := compileIntentSource(*spec.Multiplier, "multiplier")
		if err != nil {
			return IntentRules{}, err
		}
		rules.Multiplier = &mult
	}
	return rules, nil
}

func compileIntentSource(src cpopv1alpha1.IntentSourceSpec, label string) (IntentRule, error) {
	rule := IntentRule{
		Annotation: src.Annotation,
		EnvVar:     src.EnvVar,
		Default:    src.Default,
	}
	if rule.Default < 1 {
		// An omitted span or intent block skips the CRD default; zero would give span 0.
		rule.Default = 1
	}
	if src.ArgRegex != "" {
		re, err := regexp.Compile(src.ArgRegex)
		if err != nil {
			return IntentRule{}, fmt.Errorf("intent.%s.argRegex: %w", label, err)
		}
		rule.ArgRegex = re
	}
	return rule, nil
}

// extractIntent: when c is nil (pod-scoped match) every container is scanned
// and the largest value wins, so a sidecar's zero cannot clobber the workload.
func extractIntent(pod *corev1.Pod, c *corev1.Container, rule IntentRule) int {
	if pod != nil && rule.Annotation != "" {
		if v, ok := pod.Annotations[rule.Annotation]; ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 1 {
				return n
			}
		}
	}
	scan := func(container *corev1.Container) (int, bool) {
		if rule.ArgRegex != nil {
			if n, ok := extractFromArgs(container.Args, rule.ArgRegex); ok {
				return n, true
			}
		}
		if rule.EnvVar != "" {
			if n, ok := extractFromEnv(container.Env, rule.EnvVar); ok {
				return n, true
			}
		}
		return 0, false
	}
	if c != nil {
		if n, ok := scan(c); ok {
			return n
		}
		return rule.Default
	}
	if pod == nil {
		return rule.Default
	}
	best := rule.Default
	for i := range pod.Spec.Containers {
		if n, ok := scan(&pod.Spec.Containers[i]); ok && n > best {
			best = n
		}
	}
	return best
}

// extractFromArgs joins args with a space so one regex matches both "--flag N"
// and "--flag=N" without teaching the policy author two patterns.
func extractFromArgs(args []string, re *regexp.Regexp) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	m := re.FindStringSubmatch(strings.Join(args, " "))
	if m == nil {
		return 0, false
	}
	for i := 1; i < len(m); i++ {
		if m[i] == "" {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(m[i])); err == nil && n >= 1 {
			return n, true
		}
	}
	return 0, false
}

func extractFromEnv(env []corev1.EnvVar, name string) (int, bool) {
	for _, e := range env {
		if e.Name == name {
			if n, err := strconv.Atoi(strings.TrimSpace(e.Value)); err == nil && n >= 1 {
				return n, true
			}
		}
	}
	return 0, false
}

// deviceBalloonSpan is one NUMA node. The name duplicates balloons.DeviceBalloonName
// to avoid an import cycle; a test keeps them in agreement.
const (
	deviceBalloonSpan = 1
	deviceBalloonName = balloonNameBase + "-device"
)

func isStandardComputeResource(name corev1.ResourceName) bool {
	switch name {
	case corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
		return true
	}
	// hugepages-* are node memory page pools, not device resources.
	return strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix)
}

// containerRequestsDevice scans Limits as well as Requests: a device pod often
// reaches the webhook with the resource on Limits only.
func containerRequestsDevice(c *corev1.Container) bool {
	for name, q := range c.Resources.Requests {
		if !isStandardComputeResource(name) && !q.IsZero() {
			return true
		}
	}
	for name, q := range c.Resources.Limits {
		if !isStandardComputeResource(name) && !q.IsZero() {
			return true
		}
	}
	return false
}

// PodRequestsDevice also scans init containers: a restartPolicy:Always init can
// hold a device, and that request must flip the whole pod.
func PodRequestsDevice(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for i := range pod.Spec.Containers {
		if containerRequestsDevice(&pod.Spec.Containers[i]) {
			return true
		}
	}
	for i := range pod.Spec.InitContainers {
		if containerRequestsDevice(&pod.Spec.InitContainers[i]) {
			return true
		}
	}
	return false
}

// Snap rounds n up to the nearest supported span in {1,2,4,8}.
func Snap(n int) int {
	switch {
	case n <= 1:
		return 1
	case n <= 2:
		return 2
	case n <= 4:
		return 4
	default:
		return 8
	}
}

const balloonNameBase = "balloon"

// DesiredSpan is the un-clamped demand. A device pod always gets deviceBalloonSpan:
// its placement follows the device's NUMA node, not the policy.
func DesiredSpan(pod *corev1.Pod, container *corev1.Container, rules IntentRules) int {
	if PodRequestsDevice(pod) {
		return deviceBalloonSpan
	}
	span := extractIntent(pod, container, rules.Span)
	if rules.Multiplier != nil {
		span = mulIntent(span, extractIntent(pod, container, *rules.Multiplier))
	}
	return Snap(span)
}

// mulIntent saturates instead of wrapping: a wrapped product would Snap to 1,
// the narrowest balloon, instead of the widest.
func mulIntent(span, multiplier int) int {
	if span <= 0 || multiplier <= 0 {
		return 0
	}
	if span > math.MaxInt/multiplier {
		return math.MaxInt
	}
	return span * multiplier
}

// SpanForPod is DesiredSpan capped to the node's largest emitted balloon-spanN.
func SpanForPod(pod *corev1.Pod, container *corev1.Container, rules IntentRules, nodeMaxSpan int) int {
	snapped := DesiredSpan(pod, container, rules)
	if snapped > nodeMaxSpan {
		snapped = largestSupportedSpanAtMost(nodeMaxSpan)
	}
	return snapped
}

// BalloonNameForPod is shared by the webhook and the drift reconciler. The intended
// name is stamped even if the node lacks it; NRI then fails the container loudly.
func BalloonNameForPod(pod *corev1.Pod, container *corev1.Container, rules IntentRules, nodeMaxSpan int) string {
	if PodRequestsDevice(pod) {
		return deviceBalloonName
	}
	return fmt.Sprintf("%s-span%d%s",
		balloonNameBase,
		SpanForPod(pod, container, rules, nodeMaxSpan),
		balloonSuffixForCoLocate(rules.CoLocate))
}

// balloonSuffixForCoLocate is case-sensitive: the CRD enum is lowercase only.
func balloonSuffixForCoLocate(coLocate string) string {
	switch coLocate {
	case "package":
		return "-colocate"
	case "numa":
		return "-spread"
	default:
		// Do not fabricate a suffix the generator never emits.
		return ""
	}
}

// SpanFromBalloonName is the inverse of BalloonNameForPod.
func SpanFromBalloonName(name string) (span int, ok bool) {
	if name == deviceBalloonName {
		return deviceBalloonSpan, true
	}
	rest, found := strings.CutPrefix(name, balloonNameBase+"-span")
	if !found {
		return 0, false
	}
	digits, suffix, hasSuffix := strings.Cut(rest, "-")
	if hasSuffix && suffix != "colocate" && suffix != "spread" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	// Reject widths and spellings ("+2", "02") the generator never emits.
	if err != nil || n < 1 || Snap(n) != n || strconv.Itoa(n) != digits {
		return 0, false
	}
	return n, true
}

func largestSupportedSpanAtMost(n int) int {
	for _, span := range []int{8, 4, 2, 1} {
		if span <= n {
			return span
		}
	}
	return 1
}

// PinnedConfigHash is the drift key. Reserved-pool and scheduling changes do
// not alter an already-pinned cpuset, so they are excluded.
func PinnedConfigHash(balloonName, cpuAllocation string) string {
	sum := sha256.Sum256([]byte(balloonName + "|" + cpuAllocation))
	return fmt.Sprintf("%x", sum[:8])
}
