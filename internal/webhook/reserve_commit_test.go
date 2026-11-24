// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/intel/cpu-pinning-operator/internal/metrics"
	nrtpkg "github.com/intel/cpu-pinning-operator/internal/nrt"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// flakyReserver is a reserveClient double that errors on its first n calls, then answers.
type flakyReserver struct {
	failFirst int
	node      string
	accepted  bool
	keys      []string
}

func (f *flakyReserver) reserveBest(_ context.Context, key string, _ []ReserveRequest) (string, []int, bool, error) {
	f.keys = append(f.keys, key)
	if len(f.keys) <= f.failFirst {
		return "", nil, false, errors.New("reserve RPC to leader: i/o timeout")
	}
	return f.node, nil, f.accepted, nil
}

// denyMessage is resp's client-visible message, or "" when resp carries no Result.
func denyMessage(resp admission.Response) string {
	if resp.Result == nil {
		return ""
	}
	return resp.Result.Message
}

// liveReservationCount is how many unexpired reservations node n1 holds.
func liveReservationCount(s *ReservationStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pruneLocked("n1"))
}

// liveHandleMutator builds a Handle-wired mutator on the live headroom path with
// every CPU free.
func liveHandleMutator(t *testing.T, mode string) (*PodMutator, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(nrtpkg.GVK, &unstructured.Unstructured{})
	listGVK := nrtpkg.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	topo := topology.NewCache()
	topo.Set("n1", simpleTopo())
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(topoNode("n1", 4, 2), nrtObj("n1")).Build()
	return newHandleMutatorFromClient(t, mode, c, topo)
}

// keyedRequest builds a CREATE admission with an AdmissionRequest UID, which
// reserveKey derives the idempotency key from.
func keyedRequest(t *testing.T, name, uid string) admission.Request {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "inference",
			Annotations: map[string]string{"cpu-pinning-operator.intel.com/tp": "1"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
		}}},
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       types.UID(uid),
		Kind:      metav1.GroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}),
		Operation: admissionv1.Create,
		Namespace: pod.Namespace,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func admitKeyedPod(t *testing.T, m *PodMutator, name, uid string) admission.Response {
	t.Helper()
	return m.Handle(context.Background(), keyedRequest(t, name, uid))
}

// TestReserveKeyIsPerAdmissionAttempt proves the key is stable across one
// admission's retries and differs between admissions.
func TestReserveKeyIsPerAdmissionAttempt(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vllm-a"}}
	first := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "adm-1"}}

	if a, b := reserveKey(pod, first), reserveKey(pod, first); a != b {
		t.Errorf("key not stable for one admission: %q vs %q — a retry would book twice", a, b)
	}
	recreated := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vllm-a", UID: "pod-2"}}
	second := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "adm-2"}}
	if reserveKey(pod, first) == reserveKey(recreated, second) {
		t.Error("a recreated pod shares the original's key; the ledger would replay a node it never reserved")
	}
	// Vary only the admission UID.
	if a, b := reserveKey(pod, first), reserveKey(pod, second); a == b {
		t.Errorf("two admissions of one pod share key %q; the second would replay the first's node", a)
	}
	// The apiserver assigns metadata.uid after mutating admission, so a CREATE has no
	// pod UID; a pod.UID key would skip the ledger.
	if reserveKey(pod, first) == "" {
		t.Error("a CREATE admission yields no key, so no commit is ever idempotent")
	}
	// No admission UID means no key, not a shared constant.
	if k := reserveKey(pod, admission.Request{}); k != "" {
		t.Errorf("reserveKey with no admission UID = %q, want empty", k)
	}
}

// TestCommitReserveRetriesOnTheSameKey proves an error is retried on the same key;
// a fresh key would double-book.
func TestCommitReserveRetriesOnTheSameKey(t *testing.T) {
	m := &PodMutator{Reservations: NewReservationStore(0)}
	stub := &flakyReserver{failFirst: 1, node: "n1", accepted: true}
	m.SetReserver(stub)

	rp := reservePolicy{key: "pod-1/adm-1", mode: "enforce", failClosed: true}
	reqs := []ReserveRequest{{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vllm-a", Namespace: "inference"}}
	node, _, accepted, unaccountable := m.commitReserve(context.Background(), pod, rp, reqs, logr.Discard())

	if unaccountable {
		t.Fatal("commitReserve reported unaccountable although the retry answered; the retry is what buys back the availability fail-closed costs")
	}
	if !accepted || node != "n1" {
		t.Fatalf("commitReserve = (%q, %v), want (n1, true)", node, accepted)
	}
	if len(stub.keys) != 2 {
		t.Fatalf("seam called %d times, want 2 (one error, one retry)", len(stub.keys))
	}
	if stub.keys[0] != stub.keys[1] {
		t.Errorf("retry used key %q after %q; a new key makes the authority book a second reservation", stub.keys[1], stub.keys[0])
	}
	// The authority answered, so the local store stays empty.
	if r := reservedCores(m.Reservations, "n1"); len(r) != 0 {
		t.Errorf("local store = %v, want empty — an answered commit must not also reserve locally", r)
	}
}

// TestAuthorityReplaysARetriedCommit proves a follower retrying after a lost reply
// gets the leader's commit replayed: one reservation, though a second would fit.
func TestAuthorityReplaysARetriedCommit(t *testing.T) {
	store := NewReservationStore(0)
	a := &ReserveAuthority{Store: store, Log: logr.Discard()}
	reqs := []ReserveRequest{{Node: "n1", PerNUMACores: 2, Span: 1, RawFree: map[int]int{0: 4}}}
	body, err := json.Marshal(reqs)
	if err != nil {
		t.Fatal(err)
	}
	const key = "pod-1/adm-1"

	post := func() reserveResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, ReservePath, bytes.NewReader(body))
		req.Header.Set(reserveIdempotencyHeader, key)
		rr := httptest.NewRecorder()
		a.handleReserve(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("reserve returned %d, want 200", rr.Code)
		}
		var resp reserveResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode reserve response: %v", err)
		}
		return resp
	}

	committed := post()
	if !committed.Accepted || committed.Node != "n1" {
		t.Fatalf("first commit = %+v, want accepted on n1", committed)
	}
	// The follower retries the same admission.
	replayed := post()
	if !replayed.Accepted || replayed.Node != committed.Node {
		t.Fatalf("retry = %+v, want the first commit's answer (%+v) replayed", replayed, committed)
	}
	if got := liveReservationCount(store); got != 1 {
		t.Fatalf("node n1 holds %d reservations after one pod's commit and retry, want 1 — the pod is double-booked and the phantom lives until the TTL", got)
	}
}

// TestAuthorityDoesNotReplayAcrossAdmissions proves two admissions with identical
// candidates each book their own.
func TestAuthorityDoesNotReplayAcrossAdmissions(t *testing.T) {
	store := NewReservationStore(0)
	a := &ReserveAuthority{Store: store, Log: logr.Discard()}
	reqs := []ReserveRequest{{Node: "n1", PerNUMACores: 2, Span: 1, RawFree: map[int]int{0: 4}}}
	body, err := json.Marshal(reqs)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pod-1/adm-1", "pod-2/adm-2"} {
		req := httptest.NewRequest(http.MethodPost, ReservePath, bytes.NewReader(body))
		req.Header.Set(reserveIdempotencyHeader, key)
		rr := httptest.NewRecorder()
		a.handleReserve(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("reserve for %s returned %d, want 200", key, rr.Code)
		}
	}
	if got := liveReservationCount(store); got != 2 {
		t.Fatalf("node n1 holds %d reservations for two distinct admissions, want 2 — the second pod was handed the first's booking", got)
	}
}

// TestLocalReserveClientKeepsNoLedger proves an in-process reserve keeps no ledger.
func TestLocalReserveClientKeepsNoLedger(t *testing.T) {
	store := NewReservationStore(0)
	c := localReserveClient{store: store}
	req := ReserveRequest{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}
	if _, _, ok, err := c.reserveBest(context.Background(), "pod-1/adm-1", []ReserveRequest{req}); err != nil || !ok {
		t.Fatalf("local reserve: got (%v, %v), want (true, nil)", ok, err)
	}
	store.mu.Lock()
	entries := len(store.committed)
	store.mu.Unlock()
	if entries != 0 {
		t.Errorf("local store kept %d ledger entries; a single-replica install pays for idempotency it cannot need", entries)
	}
}

// TestCommitUnaccountableSplitsByMode proves enforce refuses when the seam always
// errors, while warn/off fall back locally.
func TestCommitUnaccountableSplitsByMode(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		wantDeny  bool
		wantLocal bool
	}{
		{mode: "enforce", wantDeny: true, wantLocal: false},
		{mode: "warn", wantDeny: false, wantLocal: true},
		{mode: "off", wantDeny: false, wantLocal: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			m, rec := liveHandleMutator(t, tc.mode)
			m.SetReserver(&stubReserver{err: errors.New("reserve RPC to leader: connection refused")})

			resp := admitKeyedPod(t, m, "vllm-a", "adm-1")
			if denied := !resp.Allowed; denied != tc.wantDeny {
				t.Fatalf("mode %s: denied=%v, want %v (message %q)", tc.mode, denied, tc.wantDeny, denyMessage(resp))
			}
			if gotLocal := len(reservedCores(m.Reservations, "n1")) > 0; gotLocal != tc.wantLocal {
				t.Errorf("mode %s: local reservation present=%v, want %v", tc.mode, gotLocal, tc.wantLocal)
			}
			events := drainEvents(rec)
			if hasEventContaining(events, "ReserveUnaccountable") != tc.wantDeny {
				t.Errorf("mode %s: ReserveUnaccountable event present=%v, want %v (events %v)",
					tc.mode, hasEventContaining(events, "ReserveUnaccountable"), tc.wantDeny, events)
			}
		})
	}
}

// TestUnaccountableDenialIsDistinguishableFromNoRoom proves "cannot account" is a
// different signal from "no room", with capacity labels left at zero.
func TestUnaccountableDenialIsDistinguishableFromNoRoom(t *testing.T) {
	const ns = "inference"
	// The pod is unbound, so the node label is empty by design.
	before := func(reason string) float64 {
		return testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("", ns, reason, "enforce"))
	}
	unaccountableBefore, capacityBefore, memoryBefore := before(reasonUnaccountable), before("capacity"), before("memory")

	m, rec := liveHandleMutator(t, "enforce")
	m.SetReserver(&stubReserver{err: errors.New("reserve RPC to leader: connection refused")})
	resp := admitKeyedPod(t, m, "vllm-a", "adm-1")

	if resp.Allowed {
		t.Fatal("an unaccountable commit under enforce must deny")
	}
	if got := before(reasonUnaccountable) - unaccountableBefore; got != 1 {
		t.Errorf("capacity_check_failures_total{reason=%s,mode=enforce} rose by %v, want 1", reasonUnaccountable, got)
	}
	// No capacity-labelled increment.
	if got := before("capacity") - capacityBefore; got != 0 {
		t.Errorf("reason=capacity rose by %v on an unaccountable denial, want 0 — 'no room' and 'cannot account' must not share a label", got)
	}
	if got := before("memory") - memoryBefore; got != 0 {
		t.Errorf("reason=memory rose by %v on an unaccountable denial, want 0", got)
	}
	events := drainEvents(rec)
	if !hasEventContaining(events, "ReserveUnaccountable") {
		t.Errorf("no ReserveUnaccountable event; got %v", events)
	}
	if hasEventContaining(events, "CapacityCheckFailed") {
		t.Errorf("an unaccountable commit emitted CapacityCheckFailed, which reads as a full fleet; got %v", events)
	}

	// The message must not disclose node names or fleet counts.
	msg := denyMessage(resp)
	if !strings.Contains(msg, "cannot account") {
		t.Errorf("denial message does not tell the client the pod could not be accounted for: %q", msg)
	}
	for _, leak := range []string{"n1", "balloons-enabled", "candidate"} {
		if strings.Contains(msg, leak) {
			t.Errorf("denial message discloses %q: %q", leak, msg)
		}
	}
}

// TestReserveKeyCrossesTheWire proves the key header is sent over a real mTLS
// follower-to-authority call.
func TestReserveKeyCrossesTheWire(t *testing.T) {
	certDir := writeReserveCerts(t)
	store := NewReservationStore(0)
	addr := startAuthority(t, certDir, store)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newLeaderReserveClient(certDir, atoiOrFatal(t, portStr), NewReservationStore(0),
		func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	// Span 1 on two NUMAs: a second booking takes the other NUMA, so a replay that
	// re-picked would answer differently.
	req := ReserveRequest{Node: "n1", PerNUMACores: 2, Span: 1, RawFree: map[int]int{0: 4, 1: 4}}

	first, firstNUMAs, ok, err := c.reserveBest(context.Background(), "pod-1/adm-1", []ReserveRequest{req})
	if err != nil || !ok {
		t.Fatalf("first keyed reserve over the wire: (%q, %v, %v), want accepted", first, ok, err)
	}
	// A different admission gets its own booking, on the other NUMA.
	_, otherNUMAs, ok, err := c.reserveBest(context.Background(), "pod-2/adm-2", []ReserveRequest{req})
	if err != nil || !ok || slices.Equal(otherNUMAs, firstNUMAs) {
		t.Fatalf("second admission over the wire: (%v, %v, %v), want accepted off NUMAs %v", otherNUMAs, ok, err, firstNUMAs)
	}
	// The first admission retrying after a lost reply.
	replayed, replayedNUMAs, ok, err := c.reserveBest(context.Background(), "pod-1/adm-1", []ReserveRequest{req})
	if err != nil || !ok || replayed != first || !slices.Equal(replayedNUMAs, firstNUMAs) {
		t.Fatalf("retry over the wire: (%q %v, %v, %v), want the first commit %q %v replayed", replayed, replayedNUMAs, ok, err, first, firstNUMAs)
	}
	if got := liveReservationCount(store); got != 2 {
		t.Fatalf("authority holds %d reservations for two admissions and a retry, want 2 — the key is not reaching the authority over the wire", got)
	}
}

// TestLeaderCommitsAfterFollowerTimeout proves the leader's pre-commit "client gone"
// guard cannot swallow a commit when the follower times out mid-flight. The store
// lock, held across the follower's first call, imposes the ordering.
func TestLeaderCommitsAfterFollowerTimeout(t *testing.T) {
	const key = "pod-1/adm-1"
	certDir := writeReserveCerts(t)
	leader := NewReservationStore(0)
	addr := startAuthority(t, certDir, leader)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	follower := NewReservationStore(0)
	c, err := newLeaderReserveClient(certDir, atoiOrFatal(t, portStr), follower,
		func(context.Context) (string, bool, error) { return host, false, nil }, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	// Shorter than reserveRPCTimeout, but must outlast connect + TLS handshake so
	// handleReserve reaches its client-gone sample first.
	c.httpc.Timeout = time.Second

	// A second commit would also fit, so the count separates replay from re-booking.
	reqs := []ReserveRequest{{Node: "n1", PerNUMACores: 2, Span: 1, RawFree: map[int]int{0: 4}}}

	leader.mu.Lock()
	node, _, accepted, err := c.reserveBest(context.Background(), key, reqs)
	if err == nil {
		leader.mu.Unlock()
		t.Fatalf("first attempt returned (%q, %v); the interleaving needs it to fail on the client timeout", node, accepted)
	}
	// The follower has given up; let the commit land.
	leader.mu.Unlock()
	awaitReservations(t, leader, 1, "the leader's late commit never landed, so the interleaving was not reproduced")

	replayed, _, accepted, err := c.reserveBest(context.Background(), key, reqs)
	if err != nil || !accepted {
		t.Fatalf("retry after the lost reply: (%q, %v, %v), want the leader's commit replayed", replayed, accepted, err)
	}
	if replayed != "n1" {
		t.Errorf("retry replayed node %q, want n1 — the caller pins the pod to the node it is handed", replayed)
	}
	if got := liveReservationCount(leader); got != 1 {
		t.Errorf("leader holds %d reservations for one pod, want 1 — the phantom holds per-NUMA headroom until the TTL with nothing reconciling it", got)
	}
	// Both attempts were forwarded, so the follower's own store stays empty.
	if r := reservedCores(follower, "n1"); len(r) != 0 {
		t.Errorf("follower store = %v, want empty — a forwarded reserve must not also book locally", r)
	}
}

// awaitReservations blocks until s holds want live reservations.
func awaitReservations(t *testing.T, s *ReservationStore, want int, why string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if liveReservationCount(s) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("store holds %d live reservations, want %d — %s", liveReservationCount(s), want, why)
}

// TestStoreRecordsOnlyAcceptedCommits proves a rejection is not replayed (it would
// name node "") and is re-evaluated once room frees.
func TestStoreRecordsOnlyAcceptedCommits(t *testing.T) {
	s := NewReservationStore(0)
	// No candidate fits.
	tooBig := []ReserveRequest{{Node: "n1", PerNUMACores: 8, Span: 1, RawFree: map[int]int{0: 2}}}
	if node, _, ok := s.authoritativeReserveBest("pod-1/adm-1", tooBig); ok {
		t.Fatalf("reserve = (%q, true), want a rejection", node)
	}
	s.mu.Lock()
	entries := len(s.committed)
	s.mu.Unlock()
	if entries != 0 {
		t.Errorf("ledger kept %d entries for a REJECTED commit; a replay would hand the retry a reservation on node \"\"", entries)
	}
	// Room frees up; the same key must now commit.
	fits := []ReserveRequest{{Node: "n1", PerNUMACores: 8, Span: 1, RawFree: map[int]int{0: 8}}}
	if node, _, ok := s.authoritativeReserveBest("pod-1/adm-1", fits); !ok || node != "n1" {
		t.Errorf("retry after room freed = (%q, %v), want accepted on n1 — a remembered rejection would deny a pod that now fits", node, ok)
	}
}

// TestReplayHoldsWhenTheCandidateListChanged proves a same-key retry replays even
// after the first node left the ranking, instead of booking a second reservation;
// the caller's requestedNode check turns it into an error.
func TestReplayHoldsWhenTheCandidateListChanged(t *testing.T) {
	s := NewReservationStore(0)
	const key = "pod-1/adm-1"
	onN1 := []ReserveRequest{{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}}
	onN2 := []ReserveRequest{{Node: "n2", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}}

	if node, _, ok := s.authoritativeReserveBest(key, onN1); !ok || node != "n1" {
		t.Fatalf("first commit = (%q, %v), want n1", node, ok)
	}
	// Same admission retrying, but n1 left its ranking.
	node, _, ok := s.authoritativeReserveBest(key, onN2)
	if !ok || node != "n1" {
		t.Errorf("retry = (%q, %v), want the remembered n1 replayed", node, ok)
	}
	if got := liveReservationCount(s) + reservedTotal(s, "n2"); got != 1 {
		t.Errorf("the pod holds %d reservations, want 1 — a second booking leaves the first as a phantom until the TTL", got)
	}
	// One order slot per key: a duplicate would stall the expiry sweep.
	s.mu.Lock()
	order := len(s.committedOrder)
	s.mu.Unlock()
	if order != 1 {
		t.Errorf("committedOrder holds %d slots for one key, want 1", order)
	}
}

// reservedTotal counts a named node's unexpired reservations.
func reservedTotal(s *ReservationStore, node string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pruneLocked(node))
}

// TestDryRunIsNeverDeniedByFailClosed proves an unreachable authority never refuses
// a dry-run, even under enforce.
func TestDryRunIsNeverDeniedByFailClosed(t *testing.T) {
	m, _ := liveHandleMutator(t, "enforce")
	stub := &stubReserver{err: errors.New("reserve RPC to leader: connection refused")}
	m.SetReserver(stub)

	dry := true
	req := keyedRequest(t, "vllm-a", "adm-1")
	req.DryRun = &dry
	if resp := m.Handle(context.Background(), req); !resp.Allowed {
		t.Fatalf("a dry-run must not be denied by fail-closed, got %q", denyMessage(resp))
	}
	if stub.calls != 0 {
		t.Errorf("seam consulted %d times on a dry-run, want 0", stub.calls)
	}
}

// TestLedgerEntriesExpireWithTheirReservation proves ledger entries expire with
// their reservation.
func TestLedgerEntriesExpireWithTheirReservation(t *testing.T) {
	clk := (&fakeClock{}).At(time.Unix(1_000_000, 0))
	s := NewReservationStore(0)
	s.now = clk.Now
	reqs := []ReserveRequest{{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}}

	if _, _, ok := s.authoritativeReserveBest("pod-1/adm-1", reqs); !ok {
		t.Fatal("first commit must be accepted")
	}
	clk.Advance(DefaultReservationTTL + time.Second)
	// Any keyed call sweeps; a fresh key tests expiry, not replay.
	if _, _, ok := s.authoritativeReserveBest("pod-2/adm-2", reqs); !ok {
		t.Fatal("commit after the TTL must be accepted")
	}
	s.mu.Lock()
	entries, order := len(s.committed), len(s.committedOrder)
	_, stillThere := s.committed["pod-1/adm-1"]
	s.mu.Unlock()
	if stillThere {
		t.Error("the expired admission's ledger entry survived its reservation; the ledger leaks one entry per admission")
	}
	if entries != 1 || order != 1 {
		t.Errorf("ledger holds %d entries / %d order slots after one expiry and one live commit, want 1/1", entries, order)
	}
}

// TestOverLongIdempotencyKeyIsServedWithoutTheLedger proves a junk key is served
// unkeyed, so a repeat commits again.
func TestOverLongIdempotencyKeyIsServedWithoutTheLedger(t *testing.T) {
	store := NewReservationStore(0)
	a := &ReserveAuthority{Store: store, Log: logr.Discard()}
	reqs := []ReserveRequest{{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}}
	body, err := json.Marshal(reqs)
	if err != nil {
		t.Fatal(err)
	}
	longKey := strings.Repeat("k", maxReserveKeyBytes+1)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, ReservePath, bytes.NewReader(body))
		req.Header.Set(reserveIdempotencyHeader, longKey)
		rr := httptest.NewRecorder()
		a.handleReserve(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("over-long key returned %d, want 200 — refusing the reserve costs admission, losing the replay costs a phantom", rr.Code)
		}
	}
	store.mu.Lock()
	entries := len(store.committed)
	store.mu.Unlock()
	if entries != 0 {
		t.Errorf("ledger keyed on an over-long header (%d entries); the bound is not being applied", entries)
	}
	if got := liveReservationCount(store); got != 2 {
		t.Errorf("node n1 holds %d reservations, want 2 — an unkeyed reserve must not be replayed", got)
	}
}

// TestLeaderIsSelfKeepsNoLedger proves the lease holder's own reserve is not memoised.
func TestLeaderIsSelfKeepsNoLedger(t *testing.T) {
	store := NewReservationStore(0)
	c := &leaderReserveClient{
		discover: staticDiscover(discoverResult{isSelf: true}),
		local:    store,
		port:     DefaultReservePort,
		log:      logr.Discard(),
	}
	req := ReserveRequest{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}
	if _, _, ok, err := c.reserveBest(context.Background(), "pod-1/adm-1", []ReserveRequest{req}); err != nil || !ok {
		t.Fatalf("isSelf reserve: (%v, %v), want accepted", ok, err)
	}
	store.mu.Lock()
	entries := len(store.committed)
	store.mu.Unlock()
	if entries != 0 {
		t.Errorf("the leader memoised %d in-process reserves; an in-process reserve cannot lose its reply", entries)
	}
}

// TestUnaccountableDenialIsBucketedAsError proves the failure is labelled
// result=error, not denied.
func TestUnaccountableDenialIsBucketedAsError(t *testing.T) {
	m, _ := liveHandleMutator(t, "enforce")
	m.SetReserver(&stubReserver{err: errors.New("reserve RPC to leader: connection refused")})

	resp, result := m.handle(context.Background(), keyedRequest(t, "vllm-a", "adm-1"))
	if resp.Allowed {
		t.Fatal("an unaccountable commit under enforce must deny")
	}
	if result != resultError {
		t.Errorf("admission result = %q, want %q — any other value reads as a decision the operator reached, not as its failure to account for the pod", result, resultError)
	}
}

// TestColdStartEnforceDeniesWhenTheCommitCannotBeAccounted proves the static path
// does not report the failure as reason=capacity.
func TestColdStartEnforceDeniesWhenTheCommitCannotBeAccounted(t *testing.T) {
	const ns = "inference"
	reason := func(r string) float64 {
		return testutil.ToFloat64(metrics.CapacityCheckFailures.WithLabelValues("", ns, r, "enforce"))
	}
	unaccBefore, capBefore := reason(reasonUnaccountable), reason("capacity")

	m, rec := coldStartMutator(t, "enforce", topoNode("n1", 8, 2))
	m.Reservations = NewReservationStore(0)
	m.SetReserver(&stubReserver{err: errors.New("reserve RPC to leader: connection refused")})

	resp := admitPod(t, m, "vllm-a", "2", "1")
	if resp.Allowed {
		t.Fatal("the cold-start static path must deny an unaccountable commit under enforce")
	}
	if got := reason(reasonUnaccountable) - unaccBefore; got != 1 {
		t.Errorf("reason=%s rose by %v, want 1", reasonUnaccountable, got)
	}
	if got := reason("capacity") - capBefore; got != 0 {
		t.Errorf("reason=capacity rose by %v; the static path is reporting a broken authority as a full fleet", got)
	}
	events := drainEvents(rec)
	if !hasEventContaining(events, "ReserveUnaccountable") || hasEventContaining(events, "CapacityCheckFailed") {
		t.Errorf("events = %v, want ReserveUnaccountable and no CapacityCheckFailed", events)
	}
}
