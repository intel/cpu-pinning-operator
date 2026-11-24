// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// FuzzHandleReserve posts arbitrary bodies to the reserve endpoint: the result must be a
// definite verdict or a 400, and never a reservation the verdict does not describe.
func FuzzHandleReserve(f *testing.F) {
	f.Add([]byte(`[{"node":"n1","perNumaCores":2,"span":2,"rawFree":{"0":4,"1":4}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":0,"span":0,"rawFree":{}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":-5,"span":-1,"rawFree":{"0":-1}}]`))
	f.Add([]byte(`[{"node":"","perNumaCores":1,"rawFree":{"0":1}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"perNumaMem":-1,"memBytes":9223372036854775807,` +
		`"span":1,"memChecked":true,"rawFree":{"0":8},"memFree":{"0":1}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"perNumaMem":0,"memBytes":100,` +
		`"span":1,"memChecked":true,"rawFree":{"0":8},"memFree":{"0":1024}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"span":2,"rawFree":{"0":8}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"span":1,"rawFree":{"65536":8}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"span":1,` +
		`"rawFree":{"0":8,"1":8,"2":8,"3":8,"4":8,"5":8,"6":8,"7":8}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"rawFree":{"0":8}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"memBytes":1,"rawFree":{"0":1}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"perNumaMem":-1,"memBytes":-1,` +
		`"span":1,"memChecked":true,"rawFree":{"0":8},"memFree":{"0":1}}]`))
	f.Add([]byte(`[{"node":"n1","perNumACores":1,"rAwFree":{"1":1}}]0`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"span":8,` +
		`"rawFree":{"0":8,"1":8,"2":8,"3":8,"4":8,"5":8,"6":8,"7":8}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"span":1,"rawFree":{"0":8,"1":-1}}]`))
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"perNumaMem":1,"memBytes":1,"span":1,"memChecked":true,` +
		`"rawFree":{"0":8,"1":8},"memFree":{"0":1024,"1":-1}}]`))
	f.Add(overlongNUMAListBody())
	f.Add([]byte(`[]`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[`))

	f.Fuzz(func(t *testing.T, body []byte) {
		store := NewReservationStore(0)
		a := &ReserveAuthority{Store: store, Log: logr.Discard()}

		rr := httptest.NewRecorder()
		a.handleReserve(rr, httptest.NewRequest(http.MethodPost, ReservePath, bytes.NewReader(body)))

		switch rr.Code {
		case http.StatusOK:
		case http.StatusBadRequest:
			if reservedNodes(store) != 0 {
				t.Fatalf("a rejected request (400) still reserved: %v", store.byND)
			}
			return
		default:
			t.Fatalf("unexpected status %d for body %q", rr.Code, body)
		}

		var resp reserveResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("200 response is not a decodable verdict: %v", err)
		}
		if !resp.Accepted {
			if resp.Node != "" {
				t.Fatalf("verdict rejected the request but named node %q", resp.Node)
			}
			if reservedNodes(store) != 0 {
				t.Fatalf("verdict rejected the request but the store holds reservations: %v", store.byND)
			}
			return
		}
		// An accept must name a request node and have recorded the reservation. Decode as the
		// handler does: streaming, ignoring trailing bytes.
		var reqs []ReserveRequest
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&reqs); err != nil {
			t.Fatalf("body accepted but does not decode: %v", err)
		}
		var offered []ReserveRequest
		for _, r := range reqs {
			if r.Node == resp.Node {
				offered = append(offered, r)
			}
		}
		if len(offered) == 0 {
			t.Fatalf("verdict named node %q, which the request did not offer: %q", resp.Node, body)
		}
		held := store.byND[resp.Node]
		if len(held) == 0 {
			t.Fatalf("accepted on node %q but recorded no reservation for it: %q", resp.Node, body)
		}
		// The verdict names the NUMAs it recorded.
		if !slices.Equal(held[len(held)-1].numas, resp.NUMAs) {
			t.Fatalf("verdict named NUMAs %v but recorded %v: %q", resp.NUMAs, held[len(held)-1].numas, body)
		}
		for _, h := range held {
			// A span-N balloon pins at most N NUMAs.
			if len(h.numas) > maxSupportedSpan {
				t.Fatalf("recorded a reservation naming %d NUMAs, past the widest span: %q",
					len(h.numas), body)
			}
			if len(offered) == 1 && len(h.numas) > offered[0].Span {
				t.Fatalf("recorded a span-%d reservation across %d NUMAs: %q",
					offered[0].Span, len(h.numas), body)
			}
			if h.cores <= 0 || h.memBytes < 0 {
				t.Fatalf("recorded a reservation of %d cores / %d bytes per NUMA: %q", h.cores, h.memBytes, body)
			}
			// Held shares sum per NUMA id, so a repeat would double-count.
			for i, n := range h.numas {
				if n < 0 || n > topology.MaxNUMAID {
					t.Fatalf("recorded a reservation on NUMA %d, outside [0,%d]: %q", n, topology.MaxNUMAID, body)
				}
				for _, earlier := range h.numas[:i] {
					if earlier == n {
						t.Fatalf("recorded a reservation naming NUMA %d twice (%v): %q", n, h.numas, body)
					}
				}
			}
		}
		// Replay each request alone against a store already holding a reservation: the
		// subtraction is where a negative capacity would wrap into headroom.
		for _, r := range reqs {
			loaded := NewReservationStore(0)
			loaded.reserveLocked(r.Node, []int{0, 1}, 1, 1<<20)
			if _, _, ok := loaded.authoritativeReserveBest("", []ReserveRequest{r}); !ok {
				continue
			}
			for n := range r.RawFree {
				if r.RawFree[n] < 0 || (r.MemChecked && r.MemFree[n] < 0) {
					t.Fatalf("accepted a request whose NUMA %d offers %d free cpus / %d free bytes: %q",
						n, r.RawFree[n], r.MemFree[n], body)
				}
			}
		}

		// What was recorded must be the requested demand, and the checked share when memory was
		// checked. Only checkable when one request named the node.
		if len(offered) == 1 && len(held) == 1 {
			req := offered[0]
			if held[0].cores != req.PerNUMACores || held[0].memBytes != req.PerNUMAMem {
				t.Fatalf("accepted %d cores / %d bytes per NUMA on node %q but recorded %d / %d: %q",
					req.PerNUMACores, req.PerNUMAMem, resp.Node, held[0].cores, held[0].memBytes, body)
			}
			if req.MemChecked && req.PerNUMAMem != perNUMAMemShare(req.MemBytes, req.Span) {
				t.Fatalf("accepted a memory-checked request recording %d bytes/NUMA for a checked share of %d: %q",
					req.PerNUMAMem, perNUMAMemShare(req.MemBytes, req.Span), body)
			}
		}
	})
}

// FuzzHandleReserveIdempotency posts one body twice under a fuzzed Idempotency-Key. An
// accepted commit under an effective key must replay without a second reservation; keys
// past maxReserveKeyBytes are dropped, so the replay check is gated on that.
func FuzzHandleReserveIdempotency(f *testing.F) {
	fits := []byte(`[{"node":"n1","perNumaCores":1,"span":1,"rawFree":{"0":8}}]`)
	f.Add(fits, "pod-1/adm-1")
	f.Add(fits, "")
	f.Add(fits, strings.Repeat("k", maxReserveKeyBytes))
	f.Add(fits, strings.Repeat("k", maxReserveKeyBytes+1))
	// Room for a second booking, so a re-commit is visible.
	f.Add([]byte(`[{"node":"n1","perNumaCores":2,"span":1,"rawFree":{"0":8}}]`), "pod-2/adm-2")
	f.Add([]byte(`[{"node":"n1","perNumaCores":1,"span":2,"rawFree":{"0":4,"1":4},`+
		`"perNumaMem":512,"memBytes":1024,"memChecked":true,"memFree":{"0":4096,"1":4096}}]`), "pod-3/adm-3")
	f.Add([]byte(`[]`), "k")
	f.Add([]byte(`{`), "k")

	f.Fuzz(func(t *testing.T, body []byte, key string) {
		store := NewReservationStore(0)
		a := &ReserveAuthority{Store: store, Log: logr.Discard()}

		// post returns the verdict, or ok=false on a 400.
		post := func() (resp reserveResponse, ok bool) {
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, ReservePath, bytes.NewReader(body))
			if key != "" {
				r.Header.Set(reserveIdempotencyHeader, key)
			}
			a.handleReserve(rr, r)
			if rr.Code != http.StatusOK {
				return resp, false
			}
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("200 response is not a decodable verdict: %v", err)
			}
			return resp, true
		}

		first, ok := post()
		// Only accepted commits enter the ledger.
		if !ok || !first.Accepted {
			return
		}
		held := len(store.byND[first.Node])
		second, ok := post()
		if !ok {
			t.Fatalf("a body accepted once was refused on replay: %q", body)
		}
		if key == "" || len(key) > maxReserveKeyBytes {
			return
		}
		if !reflect.DeepEqual(second, first) {
			t.Fatalf("replay under key %q answered %+v, want the first verdict %+v: %q",
				key, second, first, body)
		}
		if got := len(store.byND[first.Node]); got != held {
			t.Fatalf("replay under key %q booked again: %d reservations on %q, want %d: %q",
				key, got, first.Node, held, body)
		}
	})
}

func reservedNodes(s *ReservationStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byND)
}

// maxSupportedSpan is the widest balloon the generator emits.
const maxSupportedSpan = 8

// overlongNUMAListBody offers far more NUMAs than a node can have, with room on each.
func overlongNUMAListBody() []byte {
	rawFree := map[int]int{}
	for i := 0; i < 65; i++ {
		rawFree[i] = 8
	}
	body, err := json.Marshal([]ReserveRequest{{
		Node:         "n1",
		PerNUMACores: 1,
		// Supported span: only the size of the NUMA map can reject it.
		Span:    8,
		RawFree: rawFree,
	}})
	if err != nil {
		panic(err)
	}
	return body
}

// TestHandleReserveRejectsOversizedBody pins the request body size bound.
func TestHandleReserveRejectsOversizedBody(t *testing.T) {
	store := NewReservationStore(0)
	a := &ReserveAuthority{Store: store, Log: logr.Discard()}

	body := []byte(`[{"node":"` + strings.Repeat("n", maxReserveBodyBytes+1) + `"}]`)
	rr := httptest.NewRecorder()
	a.handleReserve(rr, httptest.NewRequest(http.MethodPost, ReservePath, bytes.NewReader(body)))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized body returned %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if n := reservedNodes(store); n != 0 {
		t.Fatalf("oversized body reserved on %d node(s)", n)
	}
}

// TestHandleReserveRejectsTooManyCandidates: the byte limit alone does not bound the
// decoded list length.
func TestHandleReserveRejectsTooManyCandidates(t *testing.T) {
	store := NewReservationStore(0)
	a := &ReserveAuthority{Store: store, Log: logr.Discard()}

	body := []byte("[" + strings.Repeat("{},", maxReserveCandidates) + "{}]")
	rr := httptest.NewRecorder()
	a.handleReserve(rr, httptest.NewRequest(http.MethodPost, ReservePath, bytes.NewReader(body)))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("a list of %d candidates returned %d, want %d", maxReserveCandidates+1, rr.Code, http.StatusBadRequest)
	}
	if n := reservedNodes(store); n != 0 {
		t.Fatalf("an over-long candidate list reserved on %d node(s)", n)
	}
}

// TestReservedAggregateSaturates: the per-NUMA reservation sum saturates instead of
// wrapping negative. The fuzzer cannot reach two reservations, so it is built directly.
func TestReservedAggregateSaturates(t *testing.T) {
	s := NewReservationStore(0)
	s.reserveLocked("n1", []int{0}, math.MaxInt/2+1, math.MaxInt64/2+1)
	s.reserveLocked("n1", []int{0}, math.MaxInt/2+1, math.MaxInt64/2+1)

	s.mu.Lock()
	cores, mem := s.reservedBothLocked("n1")
	s.mu.Unlock()

	if cores[0] != math.MaxInt {
		t.Errorf("reserved cores = %d, want the saturated %d", cores[0], math.MaxInt)
	}
	if mem[0] != math.MaxInt64 {
		t.Errorf("reserved memory = %d, want the saturated %d", mem[0], int64(math.MaxInt64))
	}
}
