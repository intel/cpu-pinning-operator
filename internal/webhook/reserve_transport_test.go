// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// discoverResult is a canned result for staticDiscover.
type discoverResult struct {
	ip     string
	isSelf bool
	err    error
}

func staticDiscover(r discoverResult) func(context.Context) (string, bool, error) {
	return func(context.Context) (string, bool, error) { return r.ip, r.isSelf, r.err }
}

// TestLeaderReserveClientIsSelfUsesLocalStore: the leader reserves against its local store.
func TestLeaderReserveClientIsSelfUsesLocalStore(t *testing.T) {
	store := NewReservationStore(0)
	c := &leaderReserveClient{
		discover: staticDiscover(discoverResult{isSelf: true}),
		local:    store,
		port:     DefaultReservePort,
		log:      logr.Discard(),
	}
	req := ReserveRequest{Node: "n1", PerNUMACores: 2, Span: 1, RawFree: map[int]int{0: 2}}
	if node, _, ok, err := c.reserveBest(context.Background(), "", []ReserveRequest{req}); err != nil || !ok || node != "n1" {
		t.Fatalf("isSelf reserve: got (%q, %v, %v), want (n1, true, nil)", node, ok, err)
	}
	if node, _, ok, err := c.reserveBest(context.Background(), "", []ReserveRequest{req}); err != nil || ok || node != "" {
		t.Fatalf("isSelf second reserve: got (%q, %v, %v), want (\"\", false, nil) — local store must reject the repeat", node, ok, err)
	}
}

// TestLeaderReserveClientDiscoveryErrorFallsBack: a discovery failure is an error, not a
// reject, and leaves the local store untouched.
func TestLeaderReserveClientDiscoveryErrorFallsBack(t *testing.T) {
	store := NewReservationStore(0)
	c := &leaderReserveClient{
		discover: staticDiscover(discoverResult{err: errors.New("no lease holder yet")}),
		local:    store,
		port:     DefaultReservePort,
		log:      logr.Discard(),
	}
	_, _, ok, err := c.reserveBest(context.Background(), "", []ReserveRequest{{Node: "n1", PerNUMACores: 1, Span: 1, RawFree: map[int]int{0: 4}}})
	if err == nil {
		t.Fatal("discovery error must be returned so the caller falls back to local, got nil")
	}
	if ok {
		t.Fatal("a transport/discovery error must report accepted=false")
	}
	if r := reservedCores(store, "n1"); len(r) != 0 {
		t.Fatalf("failed leader call must not touch the local store, got %v", r)
	}
}

// TestLocalReserveClientDelegates: the single-replica client uses the in-process store.
func TestLocalReserveClientDelegates(t *testing.T) {
	store := NewReservationStore(0)
	c := localReserveClient{store: store}
	req := ReserveRequest{Node: "n1", PerNUMACores: 2, Span: 2, RawFree: map[int]int{0: 2, 1: 2}}
	if node, _, ok, err := c.reserveBest(context.Background(), "", []ReserveRequest{req}); err != nil || !ok || node != "n1" {
		t.Fatalf("local reserve: got (%q, %v, %v), want (n1, true, nil)", node, ok, err)
	}
	if _, _, ok, _ := c.reserveBest(context.Background(), "", []ReserveRequest{req}); ok {
		t.Fatal("local reserve repeat must be rejected by the same store")
	}
}

// TestHandleReserveCancelledRequestDoesNotReserve: the authority does not commit when the
// request context is already cancelled; the follower has given up and would book it again.
func TestHandleReserveCancelledRequestDoesNotReserve(t *testing.T) {
	store := NewReservationStore(0)
	a := &ReserveAuthority{Store: store, Log: logr.Discard()}

	reqs := []ReserveRequest{{Node: "n1", PerNUMACores: 2, Span: 2, RawFree: map[int]int{0: 2, 1: 2}}}
	body, err := json.Marshal(reqs)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client already gave up (RPC timeout) before the commit
	req := httptest.NewRequest(http.MethodPost, ReservePath, bytes.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()

	a.handleReserve(rr, req)

	if r := reservedCores(store, "n1"); len(r) != 0 {
		t.Fatalf("authority reserved for a cancelled request (%v): the follower already fell back to its local store, so this is a double-reserve", r)
	}
	if rr.Code == http.StatusOK {
		t.Errorf("cancelled request returned 200; want a non-OK status so no verdict is honored, got %d", rr.Code)
	}
}

// TestHandleReserveLiveRequestReserves: a live request does commit.
func TestHandleReserveLiveRequestReserves(t *testing.T) {
	store := NewReservationStore(0)
	a := &ReserveAuthority{Store: store, Log: logr.Discard()}

	reqs := []ReserveRequest{{Node: "n1", PerNUMACores: 2, Span: 2, RawFree: map[int]int{0: 2, 1: 2}}}
	body, err := json.Marshal(reqs)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, ReservePath, bytes.NewReader(body))
	rr := httptest.NewRecorder()

	a.handleReserve(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("live request returned %d, want 200", rr.Code)
	}
	var resp reserveResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Accepted || resp.Node != "n1" {
		t.Fatalf("live request verdict = (%q, %v), want (n1, true)", resp.Node, resp.Accepted)
	}
	if r := reservedCores(store, "n1"); r[0] != 2 || r[1] != 2 {
		t.Fatalf("live request store = %v, want 2 on each NUMA", r)
	}
}

func TestHolderPodName(t *testing.T) {
	cases := map[string]string{
		"cpu-op-abc123_9f8e7d6c-uuid": "cpu-op-abc123",
		"plainhostname":               "plainhostname",
		"":                            "",
		"_leadinguuid":                "",
	}
	for in, want := range cases {
		if got := holderPodName(in); got != want {
			t.Errorf("holderPodName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReserveBodyLimitFitsTheCandidateLimit: the body limit must fit the largest candidate
// list, else the follower gets 400 and falls back locally. Each candidate carries every
// NUMA of its node, bounded by the NUMA id range the authority accepts.
func TestReserveBodyLimitFitsTheCandidateLimit(t *testing.T) {
	// The most NUMAs a node can have and the longest legal node name.
	const numas = topology.MaxNUMAID + 1
	name := strings.Repeat("n", 253)

	rawFree := make(map[int]int, numas)
	memFree := make(map[int]int64, numas)
	for i := 0; i < numas; i++ {
		rawFree[i] = 128
		memFree[i] = 1 << 40
	}
	reqs := make([]ReserveRequest, 0, maxReserveCandidates)
	for i := 0; i < maxReserveCandidates; i++ {
		reqs = append(reqs, ReserveRequest{
			Node: name, PerNUMACores: 8, PerNUMAMem: 1 << 40,
			Span: 8, MemBytes: 8 << 40, RawFree: rawFree, MemChecked: true, MemFree: memFree, Packing: packingBinpack,
		})
	}
	body, err := json.Marshal(reqs)
	if err != nil {
		t.Fatalf("marshal worst-case candidate list: %v", err)
	}
	if len(body) > maxReserveBodyBytes {
		t.Errorf("a full %d-candidate list of %d-NUMA nodes encodes to %d bytes, past the %d-byte limit: "+
			"the authority would reject a legitimate list and the follower would fall back to its local store",
			maxReserveCandidates, numas, len(body), maxReserveBodyBytes)
	}
}
