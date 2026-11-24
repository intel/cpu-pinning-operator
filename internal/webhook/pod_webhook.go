// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/intel/cpu-pinning-operator/internal/match"
	"github.com/intel/cpu-pinning-operator/internal/metrics"
	"github.com/intel/cpu-pinning-operator/internal/readiness"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// Result labels on cpu_pinning_webhook_admission_seconds. resultDenied means the operator
// refused the pod (a policy check must change); resultError means the operator failed to
// decide (the operator must be fixed), even when that failure ends in a denial.
const (
	resultPinned  = "pinned"
	resultSkipped = "skipped"
	resultDenied  = "denied"
	resultError   = "error"
)

const (
	// balloonAnnotationBase is owned by the NRI balloons plugin; never rename
	// it under the operator's own annotation prefix.
	balloonAnnotationBase = "balloon.balloons.resource-policy.nri.io"

	defaultInitContainerName  = "wait-for-balloons"
	defaultWaitTimeoutSeconds = 300

	defaultCPUAllocation = "physical-cores"
	// hyperthreadsAllocation counts capacity in logical CPUs rather than physical cores.
	hyperthreadsAllocation = "hyperthreads"

	packingSpread  = "spread"
	packingBinpack = "binpack"

	amxNodeLabel = labelPrefix + "amx"
)

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// failurePolicy is Ignore so a webhook outage never blocks unrelated pods. The chart's
// mutatingwebhook.yaml mirrors this marker and a drift guard compares them.
//+kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=mpod.kb.io,admissionReviewVersions=v1,timeoutSeconds=10

// PodMutator stamps the balloon annotation and injects wait-for-balloons.
type PodMutator struct {
	Client client.Client
	// NodeReader is the balloons-enabled Node cache; m.Client stays unscoped so the Node
	// reconciler can provision unlabeled nodes. Nil (tests) falls back to Client.
	NodeReader  client.Reader
	Log         logr.Logger
	PolicyIndex *match.Index

	Recorder record.EventRecorder
	// TopoCache may be nil; capacity check then uses the static per-NUMA formula.
	TopoCache *topology.Cache
	// Reservations closes the NRT-lag burst race. Nil skips headroom steering.
	Reservations *ReservationStore
	// CertDir holds the serving cert whose ca.crt pre-flight verifies against.
	CertDir string
	// reserver is the HA reserve seam. Nil reserves against the local store.
	reserver reserveClient
	decoder  admission.Decoder

	readyMu       sync.Mutex
	readyClient   *http.Client
	readyEndpoint string
}

// SetReserver installs the HA leader-forwarding reserve seam; unset, the mutator reserves
// against its local store.
func (m *PodMutator) SetReserver(r reserveClient) { m.reserver = r }

func (m *PodMutator) reserveClientOrLocal() reserveClient {
	if m.reserver != nil {
		return m.reserver
	}
	return localReserveClient{store: m.Reservations}
}

// nodeReservations takes ReservationStore.mu; never call it with that lock held.
func (m *PodMutator) nodeReservations(node string) (cores map[int]int, mem map[int]int64) {
	if m.Reservations == nil || node == "" {
		return nil, nil
	}
	m.Reservations.mu.Lock()
	defer m.Reservations.mu.Unlock()
	return m.Reservations.reservedBothLocked(node)
}

// nodeReader is the scoped Node cache when wired. A non-balloons node is NotFound here,
// treated as unknown, never a denial.
func (m *PodMutator) nodeReader() client.Reader {
	if m.NodeReader != nil {
		return m.NodeReader
	}
	return m.Client
}

// podRef returns the pod name, or generateName when the name is not set yet (CREATE).
func podRef(pod *corev1.Pod) string {
	if pod.Name != "" {
		return pod.Name
	}
	return pod.GenerateName
}

func (m *PodMutator) eventf(pod *corev1.Pod, reason, format string, args ...interface{}) {
	if m.Recorder != nil {
		m.Recorder.Eventf(pod, corev1.EventTypeWarning, reason, format, args...)
	}
}

// denyEnforced refuses a matched pod under enforcePinning. Denied, never Errored: Errored
// returns 500 with the Go error string to the client.
func (m *PodMutator) denyEnforced(pod *corev1.Pod, detail string, err error) admission.Response {
	if err != nil {
		m.Log.Error(err, "matched pod denied: enforcePinning cannot be satisfied",
			"pod", podRef(pod), "namespace", pod.Namespace, "detail", detail)
	} else {
		m.Log.Info("matched pod denied: enforcePinning cannot be satisfied",
			"pod", podRef(pod), "namespace", pod.Namespace, "detail", detail)
	}
	m.eventf(pod, "PinningDenied", "denied %s: %s", podRef(pod), detail)
	return admission.Denied("cpu-pinning enforcePinning cannot be satisfied: " + detail)
}

func deniedCapacity(clause string) admission.Response {
	return admission.Denied("cpu-pinning capacity check failed: " + clause +
		"; see the operator log for the node-level detail")
}

const reasonUnaccountable = "unaccountable"

// denyUnaccountable refuses a pod whose placement the reservation authority could not
// commit; it counts as resultError since the authority, not the pod, is at fault.
func (m *PodMutator) denyUnaccountable(pod *corev1.Pod, log logr.Logger) admission.Response {
	log.Info("reserve could not be accounted — denying (enforce)")
	m.eventf(pod, "ReserveUnaccountable",
		"denied %s: the reservation authority could not commit this pod's placement, so the fleet cannot account for it", podRef(pod))
	metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, reasonUnaccountable, "enforce").Inc()
	return admission.Denied("cpu-pinning cannot account for this pod: the reservation authority is unavailable" +
		"; see the operator log")
}

func deniedPreflight() admission.Response {
	return admission.Denied("cpu-pinning pre-flight check failed: the balloons plugin is not " +
		"ready on this pod's node; see the operator log for the node-level detail")
}

func noRoomFor(reason string, span int) string {
	dimension := "CPU"
	if reason == "memory" {
		dimension = "memory"
	}
	return fmt.Sprintf("no room for this pod's %s request at span %d", dimension, span)
}

// abortMatched ends a matched pod whose mutation aborted. Errored is an Allowed:false the
// apiserver treats as a denial, and failurePolicy only covers call-level failures.
func (m *PodMutator) abortMatched(pod *corev1.Pod, enforce bool, reason string, err error) (admission.Response, string) {
	if enforce {
		return m.denyEnforced(pod, "pinning aborted on an internal error at stage "+reason, err), resultError
	}
	metrics.WebhookBypasses.WithLabelValues(pod.Namespace, reason).Inc()
	m.Log.Error(err, "matched pod admitted unmutated (in-band fail-open on internal error)",
		"pod", podRef(pod), "namespace", pod.Namespace, "reason", reason)
	m.eventf(pod, "WebhookBypass", "matched pod admitted unmutated after internal error: %s", reason)
	return admission.Allowed("matched pod admitted unmutated after internal error"), resultError
}

// Handle times the admission and records cpu_pinning_webhook_admission_seconds by
// result, delegating the decision to handle.
func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	start := time.Now()
	resp, result := m.handle(ctx, req)
	metrics.WebhookAdmissionSeconds.WithLabelValues(result).Observe(time.Since(start).Seconds())
	return resp
}

func (m *PodMutator) handle(ctx context.Context, req admission.Request) (admission.Response, string) {
	// These abort before match, so enforcePinning is unknown: fail open.
	pod := &corev1.Pod{}
	if m.decoder == nil {
		m.Log.Error(nil, "decoder not initialized — admitting pod unmutated (in-band fail-open)")
		metrics.WebhookBypasses.WithLabelValues(req.Namespace, metrics.BypassDecode).Inc()
		return admission.Allowed("decoder not initialized — admitting unmutated"), resultError
	}
	if err := m.decoder.Decode(req, pod); err != nil {
		// The apiserver already decoded this object; a failure here hits every request.
		m.Log.Error(err, "pod decode failed — admitting pod unmutated (in-band fail-open)")
		metrics.WebhookBypasses.WithLabelValues(req.Namespace, metrics.BypassDecode).Inc()
		return admission.Allowed("pod decode failed — admitting unmutated"), resultError
	}
	if m.PolicyIndex == nil {
		m.Log.Error(nil, "policy index not initialized — admitting pod unmutated (in-band fail-open)")
		metrics.WebhookBypasses.WithLabelValues(pod.Namespace, metrics.BypassPolicyIndex).Inc()
		return admission.Allowed("policy index not initialized — admitting unmutated"), resultError
	}

	log := m.Log.WithValues("pod", podRef(pod), "namespace", pod.Namespace)

	res := m.PolicyIndex.Match(pod)
	if res.Rule == nil {
		return admission.Allowed("pod is not a pinning target"), resultSkipped
	}
	if res.Ambiguous {
		log.Info("match ambiguous — not pinned", "reason", res.Reason)
		m.eventf(pod, "NotPinned", "matched pod left unpinned: %s", res.Reason)
		metrics.PodsSkipped.WithLabelValues(pod.Namespace, metrics.ReasonAmbiguous).Inc()
		return admission.Allowed("ambiguous container match — not pinned"), resultSkipped
	}
	rule := res.Rule
	container := res.Container

	podScoped := res.PodScoped

	if podScoped {
		if !hasWholeCorePodCPU(pod) {
			log.Info("pod CPU request is fractional, unset, or not equal to limits — not pinned")
			m.eventf(pod, "NotPinned", "matched pod left unpinned: no container has a whole-core guaranteed CPU request (requests==limits, integer cores)")
			metrics.PodsSkipped.WithLabelValues(pod.Namespace, metrics.ReasonFractional).Inc()
			return admission.Allowed("fractional or unset pod CPU request — not pinned"), resultSkipped
		}
	} else if !hasWholeCoreGuaranteedCPU(container) {
		log.Info("CPU request is fractional, unset, or not equal to limits — not pinned")
		m.eventf(pod, "NotPinned", "container %q left unpinned: CPU request is fractional, unset, or not equal to limits", container.Name)
		metrics.PodsSkipped.WithLabelValues(pod.Namespace, metrics.ReasonFractional).Inc()
		return admission.Allowed("fractional or unset CPU request — not pinned"), resultSkipped
	}

	var nodeMaxSpan int
	// boundLabels is the one read of a bound pod's node; every decision below uses it so
	// they all see the same node state. Nil for an unbound pod.
	var boundLabels map[string]string
	if pod.Spec.NodeName != "" {
		maxSpan, enabled, labels, err := m.nodeMaxSpan(ctx, pod.Spec.NodeName)
		if err != nil {
			return m.abortMatched(pod, rule.EnforcePinning, metrics.BypassNodeLookup, err)
		}
		if !enabled {
			log.Info("target node not yet provisioned for balloons — not pinned", "node", pod.Spec.NodeName)
			m.eventf(pod, "NotPinned", "left unpinned: target node %q is not balloons-enabled (excluded or not yet provisioned)", pod.Spec.NodeName)
			metrics.PodsSkipped.WithLabelValues(pod.Namespace, metrics.ReasonExcluded).Inc()
			return admission.Allowed("target node not balloons-enabled — not pinned"), resultSkipped
		}
		nodeMaxSpan = maxSpan
		boundLabels = labels
	} else {
		clusterMaxSpan, err := m.clusterMaxSpan(ctx)
		if err != nil {
			return m.abortMatched(pod, rule.EnforcePinning, metrics.BypassNodeLookup, err)
		}
		if clusterMaxSpan == 0 {
			// No BalloonsPolicy exists; stamping an annotation would name a missing balloon.
			if rule.EnforcePinning {
				return m.denyEnforced(pod, "no balloons-enabled node in the cluster yet", nil), resultDenied
			}
			log.Info("no balloons-enabled node in cluster yet — not pinned")
			m.eventf(pod, "NotPinned", "left unpinned: no balloons-enabled node in the cluster yet")
			metrics.PodsSkipped.WithLabelValues(pod.Namespace, metrics.ReasonNoBalloon).Inc()
			return admission.Allowed("no balloons-enabled node — not pinned"), resultSkipped
		}
		nodeMaxSpan = clusterMaxSpan
	}

	isDevice := match.PodRequestsDevice(pod)
	balloonName := match.BalloonNameForPod(pod, container, rule.Intent, nodeMaxSpan)
	span := match.SpanForPod(pod, container, rule.Intent, nodeMaxSpan)
	desiredSpan := match.DesiredSpan(pod, container, rule.Intent)
	variant := variantSuffix(balloonName)

	// clusterAllocation is the fleet default, used only for a node with no published mode
	// (overrides can give nodes their own cpuAllocation).
	clusterAllocation := m.PolicyIndex.CPUAllocation()
	if clusterAllocation == "" {
		clusterAllocation = defaultCPUAllocation
	}

	// pinnedAllocation is the mode the pod runs under, recorded in the drift hash. A bound pod
	// takes its node's mode; an unbound pod takes its committed node's or the uniform fleet
	// mode. Otherwise it stays unresolved rather than the fleet default, which would read as
	// permanent drift on a node under a mode override.
	pinnedAllocation := ""
	if boundLabels != nil {
		pinnedAllocation = m.nodeCPUAllocation(boundLabels, clusterAllocation)
	}

	packing := m.PolicyIndex.Packing()
	if packing != packingBinpack {
		packing = packingSpread
	}

	if desiredSpan > nodeMaxSpan && rule.CapacityCheckMode != "off" {
		var detail string
		if pod.Spec.NodeName != "" {
			detail = fmt.Sprintf("workload requests span %d but target node %q supports at most span %d",
				desiredSpan, pod.Spec.NodeName, nodeMaxSpan)
		} else {
			detail = fmt.Sprintf("workload requests span %d but no balloons-enabled node supports more than span %d",
				desiredSpan, nodeMaxSpan)
		}
		if rule.CapacityCheckMode == "enforce" {
			log.Info("topology gate failed — denying (enforce)", "detail", detail, "desiredSpan", desiredSpan, "nodeMaxSpan", nodeMaxSpan)
			m.eventf(pod, "CapacityCheckFailed", "denied: %s", detail)
			metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, "topology", "enforce").Inc()
			return deniedCapacity(fmt.Sprintf("the requested span %d is not supported", desiredSpan)), resultDenied
		}
		log.Info("topology gate failed — pinning at capped span (warn)", "detail", detail, "cappedSpan", span)
		m.eventf(pod, "CapacityCheckWarning", "pinned at span %d despite span %d request: %s", span, desiredSpan, detail)
		metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, "topology", "warn").Inc()
	}

	memBytes := podMemoryRequestBytes(pod, container, podScoped)
	memGateFailed := false
	if rule.CapacityCheckMode != "off" && memBytes > 0 {
		if fits, detail, err := m.memoryCapacityCheck(ctx, pod, span, memBytes); err != nil {
			return m.abortMatched(pod, rule.EnforcePinning, metrics.BypassCapacityCheck, err)
		} else if !fits {
			if rule.CapacityCheckMode == "enforce" {
				log.Info("memory capacity check failed — denying (enforce)", "detail", detail)
				m.eventf(pod, "CapacityCheckFailed", "denied: %s", detail)
				metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, "memory", "enforce").Inc()
				return deniedCapacity(noRoomFor("memory", span)), resultDenied
			}
			log.Info("memory capacity check failed — pinning anyway (warn)", "detail", detail)
			m.eventf(pod, "CapacityCheckWarning", "pinned despite memory concern: %s", detail)
			metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, "memory", "warn").Inc()
			memGateFailed = true
		}
	}

	// Read once so pre-flight and the init container's --endpoint cannot disagree.
	readyEndpoint := getEnv("BALLOONS_READY_ENDPOINT", "")
	skipInitNotReady := false

	// Pre-flight a scheduled pod before reserve: an enforce denial must book no headroom.
	if readyEndpoint != "" && pod.Spec.NodeName != "" {
		if ready, checked := m.checkPluginReady(ctx, readyEndpoint, pod.Spec.NodeName); checked && !ready {
			metrics.PluginNotReady.WithLabelValues(pod.Spec.NodeName).Inc()
			detail := fmt.Sprintf("balloons plugin not ready on node %q", pod.Spec.NodeName)
			switch rule.CapacityCheckMode {
			case "enforce":
				log.Info("pre-flight check failed — denying", "node", pod.Spec.NodeName)
				m.eventf(pod, "PluginNotReady", "denied: %s", detail)
				return deniedPreflight(), resultDenied
			case "off":
				// Init is still injected; the workload waits instead of starting unpinned.
			case "warn":
				// warn drops the gate; enforcePinning keeps it.
				if rule.EnforcePinning {
					log.Info("pre-flight check failed — keeping the readiness gate (enforcePinning)", "node", pod.Spec.NodeName)
					m.eventf(pod, "PluginNotReady", "pinned and held in Init: %s", detail)
					break
				}
				log.Info("pre-flight check failed — pinning without init container", "node", pod.Spec.NodeName)
				m.eventf(pod, "PluginNotReady", "pinned without init container: %s", detail)
				skipInitNotReady = true
			}
		}
	}

	rp := reservePolicy{
		dryRun:     req.DryRun != nil && *req.DryRun,
		key:        reserveKey(pod, req),
		mode:       rule.CapacityCheckMode,
		failClosed: rule.CapacityCheckMode == "enforce",
	}

	// filterMem: may memory narrow placement? memBytes always reaches the reservation either way.
	// memGateFailed already found no fit.
	filterMem := rule.CapacityCheckMode != "off" && !memGateFailed

	if pod.Spec.NodeName != "" {
		// capacityWarned: the warn gate already incremented; don't double-count at commit.
		capacityWarned := false
		if rule.CapacityCheckMode != "off" {
			if fits, detail, err := m.capacityFits(ctx, pod, container, podScoped, span, variant, clusterAllocation, boundLabels, requiredFeatureLabels(rule.NodeFeatures)); err != nil {
				return m.abortMatched(pod, rule.EnforcePinning, metrics.BypassCapacityCheck, err)
			} else if !fits {
				if rule.CapacityCheckMode == "enforce" {
					log.Info("capacity check failed — denying (enforce)", "detail", detail)
					m.eventf(pod, "CapacityCheckFailed", "denied: %s", detail)
					metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, "capacity", "enforce").Inc()
					return deniedCapacity(noRoomFor("capacity", span)), resultDenied
				}
				log.Info("capacity check failed — pinning anyway (warn)", "detail", detail)
				m.eventf(pod, "CapacityCheckWarning", "pinned despite capacity concern: %s", detail)
				metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, "capacity", "warn").Inc()
				capacityWarned = true
			}
		}
		// Dry-run skips the reservation to keep sideEffects: None.
		if m.Reservations != nil {
			cores := podCPURequestCores(pod, container, podScoped)
			perNUMA := perNUMACPUShare(cores, span)
			memPerNUMA := perNUMAMemShare(memBytes, span)
			if !rp.dryRun && cores > 0 {
				// Count reserved room in this node's allocation unit, the same one the gates used.
				rawFree, ok := m.nodeLiveFreeRaw(ctx, pod.Spec.NodeName, pinnedAllocation, m.densityNode(boundLabels))
				if !ok {
					rawFree, _ = nodeStaticFree(boundLabels, pinnedAllocation, nil)
				}
				// RawFree stays raw: the authority picks the NUMAs net of its own ledger.
				if len(rawFree) > 0 {
					req := ReserveRequest{
						Node:         pod.Spec.NodeName,
						PerNUMACores: perNUMA,
						PerNUMAMem:   memPerNUMA,
						Span:         span,
						MemBytes:     memBytes,
						RawFree:      rawFree,
					}
					// The authority re-checks memory only when MemChecked is set; without it two racing
					// memory-heavy pods both commit past the floor.
					if filterMem && memBytes > 0 {
						if rawMemFree, ok := m.nodeLiveMemFreeRaw(ctx, pod.Spec.NodeName); ok {
							req.MemChecked = true
							req.MemFree = rawMemFree
						} else if staticMemFree, ok := nodeStaticMemFree(boundLabels, nil); ok {
							req.MemChecked = true
							req.MemFree = staticMemFree
						}
					}
					reserveNode, numas, accepted, unaccountable := m.commitReserve(ctx, pod, rp, []ReserveRequest{req}, log)
					if unaccountable {
						return m.denyUnaccountable(pod, log), resultError
					}
					switch {
					case accepted:
						metrics.ReservationsRecorded.WithLabelValues(reserveNode).Inc()
						log.Info("pre-scheduled pod: recorded reservation",
							"node", pod.Spec.NodeName, "numas", numas, "cores", cores, "perNUMAShare", perNUMA,
							"memBytes", memBytes, "memPerNUMAShare", memPerNUMA, "span", span)
					default:
						// This commit is the serialization point: overlapping admissions can both pass the gate,
						// only one wins the authority. Re-test memory so a memory denial isn't labelled CPU.
						reason := "capacity"
						detail := fmt.Sprintf("request of %d cores (%d per NUMA at span %d) does not fit node %q once outstanding reservations are counted (%s)",
							cores, perNUMA, span, pod.Spec.NodeName, pinnedAllocation)
						if req.MemChecked {
							reserved, freshMem := m.nodeReservations(pod.Spec.NodeName)
							if p, ok := placeOnNode(rawFree, reserved, req.MemFree, freshMem, perNUMA, span, memBytes, ""); ok && !p.memFits {
								reason = "memory"
								detail = fmt.Sprintf("memory request of %s (%s per NUMA at span %d) does not fit node %q once outstanding reservations are counted",
									humanBytes(memBytes), humanBytes(memPerNUMA), span, pod.Spec.NodeName)
							}
						}
						if rule.CapacityCheckMode == "enforce" {
							log.Info("pre-scheduled pod: node contended on the authoritative store — denying (enforce)",
								"detail", detail,
								"node", pod.Spec.NodeName, "cores", cores, "perNUMAShare", perNUMA,
								"memBytes", memBytes, "memPerNUMAShare", memPerNUMA, "span", span)
							m.eventf(pod, "CapacityCheckFailed", "denied: %s", detail)
							metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, reason, "enforce").Inc()
							return deniedCapacity(noRoomFor(reason, span)), resultDenied
						}
						// warn/off never deny. Demand goes unrecorded; NRT heals once the pin lands.
						log.Info("pre-scheduled pod: node contended on the authoritative store, reservation not recorded",
							"node", pod.Spec.NodeName, "cores", cores, "perNUMAShare", perNUMA,
							"memBytes", memBytes, "memPerNUMAShare", memPerNUMA, "span", span)
						// One shortfall, one increment; only a CPU shortfall can have been counted by the gate.
						alreadyWarned := reason == "capacity" && capacityWarned
						if rule.CapacityCheckMode == "warn" && !alreadyWarned {
							m.eventf(pod, "CapacityCheckWarning", "pinned despite %s concern: %s", reason, detail)
							metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, reason, "warn").Inc()
						}
					}
				} else {
					// No set to charge.
					log.Info("pre-scheduled pod: no per-NUMA capacity resolved, skipping reservation",
						"node", pod.Spec.NodeName, "cores", cores, "memBytes", memBytes, "span", span)
				}
			}
		}
	} else {
		cores := podCPURequestCores(pod, container, podScoped)
		verdict := m.admitWithHeadroom(ctx, pod, cores, span, memBytes, filterMem, variant, clusterAllocation, packing, requiredFeatureLabels(rule.NodeFeatures), rp, log)
		// Steering pins the pod to one node, so that node's mode is the pod's. Empty when nothing
		// was committed.
		pinnedAllocation = verdict.cpuAllocation
		switch {
		case verdict.unaccountable:
			return m.denyUnaccountable(pod, log), resultError
		case verdict.sawLive && !verdict.fits:
			reason := verdict.reason
			if reason == "" {
				reason = "capacity"
			}
			switch rule.CapacityCheckMode {
			case "enforce":
				log.Info("capacity check failed — denying (enforce, unscheduled)", "detail", verdict.detail, "reason", reason)
				m.eventf(pod, "CapacityCheckFailed", "denied: %s", verdict.detail)
				metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, reason, "enforce").Inc()
				return deniedCapacity(noRoomFor(reason, span)), resultDenied
			case "warn":
				log.Info("capacity check failed — pinning anyway (warn, unscheduled)", "detail", verdict.detail, "reason", reason)
				m.eventf(pod, "CapacityCheckWarning", "pinned despite %s concern: %s", reason, verdict.detail)
				metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, reason, "warn").Inc()
			case "off":
			}
		case !verdict.sawLive && rule.CapacityCheckMode == "enforce":
			// Cold start: reservation-backed static check closes the burst-admit race.
			sv := m.admitStaticHeadroom(ctx, pod, cores, span, memBytes, filterMem, variant, clusterAllocation, packing, requiredFeatureLabels(rule.NodeFeatures), rp, log)
			pinnedAllocation = sv.cpuAllocation
			switch {
			case sv.unaccountable:
				return m.denyUnaccountable(pod, log), resultError
			case sv.sawStatic && !sv.fits:
				reason := sv.reason
				if reason == "" {
					reason = "capacity"
				}
				log.Info("capacity check failed — denying (enforce, static fallback)", "detail", sv.detail, "reason", reason)
				m.eventf(pod, "CapacityCheckFailed", "denied: %s", sv.detail)
				metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, reason, "enforce").Inc()
				return deniedCapacity(noRoomFor(reason, span)), resultDenied
			case sv.sawStatic && sv.fits:
			case !sv.sawStatic:
				if fits, detail, err := m.capacityFits(ctx, pod, container, podScoped, span, variant, clusterAllocation, boundLabels, requiredFeatureLabels(rule.NodeFeatures)); err != nil {
					return m.abortMatched(pod, rule.EnforcePinning, metrics.BypassCapacityCheck, err)
				} else if !fits {
					log.Info("capacity check failed — denying (enforce, static fallback, no live data)", "detail", detail)
					m.eventf(pod, "CapacityCheckFailed", "denied: %s", detail)
					metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, "capacity", "enforce").Inc()
					return deniedCapacity(noRoomFor("capacity", span)), resultDenied
				}
			}
		case !verdict.sawLive && rule.CapacityCheckMode == "warn":
			if fits, detail, err := m.capacityFits(ctx, pod, container, podScoped, span, variant, clusterAllocation, boundLabels, requiredFeatureLabels(rule.NodeFeatures)); err != nil {
				return m.abortMatched(pod, rule.EnforcePinning, metrics.BypassCapacityCheck, err)
			} else if !fits {
				log.Info("capacity check failed — pinning anyway (warn, static fallback)", "detail", detail)
				m.eventf(pod, "CapacityCheckWarning", "pinned despite capacity concern: %s", detail)
				metrics.CapacityCheckFailures.WithLabelValues(pod.Spec.NodeName, pod.Namespace, "capacity", "warn").Inc()
			}
		}
		if pinnedAllocation == "" {
			// No node committed; nodes the pod can land on that share one mode still answer.
			pinnedAllocation = m.fleetCPUAllocation(ctx, pod, requiredFeatureLabels(rule.NodeFeatures), clusterAllocation)
		}
	}

	annotationKey := fmt.Sprintf("%s/pod", balloonAnnotationBase)
	if !podScoped {
		annotationKey = fmt.Sprintf("%s/container.%s", balloonAnnotationBase, container.Name)
	}

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annotationKey] = balloonName
	pod.Annotations[PinnedByAnnotation] = PinnedByValue
	pod.Annotations[PinnedHashAnnotation] = match.PinnedConfigHash(balloonName, pinnedAllocation)

	injectedInit := false
	initImage := getEnv("INIT_CONTAINER_IMAGE", "")
	if readyEndpoint == "" {
		// Empty endpoint would wedge the init container. Pod stays pinned; gate dropped.
		log.Info("BALLOONS_READY_ENDPOINT is empty — admitting pinned pod WITHOUT the wait-for-balloons init container (readiness gate disabled)")
	} else if initImage == "" {
		// No poller image configured. Pod stays pinned; gate dropped.
		log.Info("INIT_CONTAINER_IMAGE is empty — admitting pinned pod WITHOUT the wait-for-balloons init container (readiness gate disabled)")
	} else if skipInitNotReady {
	} else if !hasInitContainer(pod) {
		pod.Spec.InitContainers = append([]corev1.Container{createInitContainer(readyEndpoint, initImage)}, pod.Spec.InitContainers...)
		injectReadinessCAVolume(pod, readyEndpoint)
		injectedInit = true
	}

	injectNodeAffinity(pod, rule.EnforcePinning, rule.NodeFeatures)

	if pod.Spec.NodeName == "" && span > 1 {
		injectMaxSpanAffinity(pod, span)
	}

	// Empty allocationMode means no node was resolved, so the mode is not hashed.
	log.Info("stamped balloon annotation",
		"balloon", balloonName, "annotation", annotationKey,
		"allocationMode", pinnedAllocation,
		"devicePod", isDevice,
		"waitForBalloonsInit", injectedInit,
		"enforcePinning", rule.EnforcePinning, "nodeFeatures", rule.NodeFeatures)
	if m.Recorder != nil {
		m.Recorder.Eventf(pod, corev1.EventTypeNormal, "Pinned",
			"assigned balloon %q via %s", balloonName, annotationKey)
	}
	metrics.PodsPinned.WithLabelValues(pod.Namespace, balloonName, rule.Name).Inc()

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return m.abortMatched(pod, rule.EnforcePinning, metrics.BypassMarshal, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod), resultPinned
}

// InjectDecoder injects the admission decoder into the PodMutator.
func (m *PodMutator) InjectDecoder(d admission.Decoder) error {
	m.decoder = d
	return nil
}

const pluginReadinessCheckTimeout = 2 * time.Second

// readinessClient matches the init container's client, verifying against ca.crt in CertDir
// (re-read per request). Reused across admissions for keep-alive.
func (m *PodMutator) readinessClient(endpoint string) (*http.Client, error) {
	m.readyMu.Lock()
	defer m.readyMu.Unlock()
	if m.readyClient == nil || m.readyEndpoint != endpoint {
		c, err := readiness.Client(endpoint, filepath.Join(m.CertDir, caCertName))
		if err != nil {
			return nil, err
		}
		m.readyClient, m.readyEndpoint = c, endpoint
	}
	return m.readyClient, nil
}

// checkPluginReady: checked is true only for a definitive answer (200, or 503 with
// {"ready":false} and no error). A 503 with an error is inconclusive and falls through to
// the init container.
func (m *PodMutator) checkPluginReady(ctx context.Context, endpoint, node string) (ready bool, checked bool) {
	reqCtx, cancel := context.WithTimeout(ctx, pluginReadinessCheckTimeout)
	defer cancel()

	httpClient, err := m.readinessClient(endpoint)
	if err != nil {
		return false, false
	}

	base := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(base, readiness.Path) {
		base += readiness.Path
	}
	target := base + "?node=" + url.QueryEscape(node)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return false, false
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, true
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		var body struct {
			Ready bool   `json:"ready"`
			Error string `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 256)).Decode(&body); err == nil && !body.Ready && body.Error == "" {
			return false, true
		}
	}
	return false, false
}
