// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// scriptedWatch is a scripted apiserver: Get returns current, each Watch replays the
// next stream and closes, recording the resourceVersion it was opened at.
type scriptedWatch struct {
	client.WithWatch
	current *unstructured.Unstructured
	gets    int
	opened  []string
	streams [][]watch.Event
}

func (s *scriptedWatch) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	s.gets++
	s.current.DeepCopyInto(obj.(*unstructured.Unstructured))
	return nil
}

func (s *scriptedWatch) Watch(_ context.Context, _ client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
	s.opened = append(s.opened, (&client.ListOptions{}).ApplyOptions(opts).AsListOptions().ResourceVersion)
	if len(s.streams) == 0 {
		return nil, errors.New("script exhausted")
	}
	evs := s.streams[0]
	s.streams = s.streams[1:]
	fw := watch.NewFakeWithChanSize(len(evs), false)
	for _, ev := range evs {
		fw.Action(ev.Type, ev.Object)
	}
	fw.Stop()
	return fw, nil
}

func bpRev(rv string, gen int64) *unstructured.Unstructured {
	u := newUnstructured(bpGVK)
	u.SetResourceVersion(rv)
	u.SetGeneration(gen)
	return u
}

// TestWatchRevisionsKeepsItsCursor proves the watch resumes from the last delivered
// revision even when the callback re-versioned its copy, and re-reads only on an
// expired history, reported as a gap.
func TestWatchRevisionsKeepsItsCursor(t *testing.T) {
	key := client.ObjectKey{Namespace: "ns", Name: "node.n1"}
	expired := watch.Event{Type: watch.Error, Object: &metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired}}
	for _, tc := range []struct {
		name       string
		fromRV     string
		streams    [][]watch.Event
		wantOpened []string
		wantGets   int
		wantGap    bool
	}{
		{
			name:   "starts at the write and resumes from the delivered revision",
			fromRV: "10",
			streams: [][]watch.Event{
				{{Type: watch.Modified, Object: bpRev("11", 6)}},
				{{Type: watch.Modified, Object: bpRev("12", 7)}},
			},
			wantOpened: []string{"10", "11"},
		},
		{
			name:       "expired history is re-read and reported",
			fromRV:     "10",
			streams:    [][]watch.Event{{expired}, {{Type: watch.Modified, Object: bpRev("51", 7)}}},
			wantOpened: []string{"10", "50"},
			wantGets:   1,
			wantGap:    true,
		},
		{
			name:       "no write to start from reads the object first",
			streams:    [][]watch.Event{{{Type: watch.Modified, Object: bpRev("51", 7)}}},
			wantOpened: []string{"50"},
			wantGets:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wc := &scriptedWatch{current: bpRev("50", 5), streams: tc.streams}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			decided, gap, err := watchRevisions(ctx, wc, key, tc.fromRV, func(u *unstructured.Unstructured) bool {
				// What the probe does on a restore: patch the object it was handed.
				u.SetResourceVersion("999")
				return u.GetGeneration() == 7
			})
			if err != nil || !decided {
				t.Fatalf("decided=%v err=%v, want the generation-7 revision", decided, err)
			}
			if !slices.Equal(wc.opened, tc.wantOpened) || wc.gets != tc.wantGets || gap != tc.wantGap {
				t.Errorf("watches opened at %v, %d read(s), gap=%v; want %v, %d, %v",
					wc.opened, wc.gets, gap, tc.wantOpened, tc.wantGets, tc.wantGap)
			}
		})
	}
}

// TestWatchRevisionsReturnsStreamedErrors proves a streamed non-410 error reaches the
// caller instead of being re-read as a gap.
func TestWatchRevisionsReturnsStreamedErrors(t *testing.T) {
	internal := watch.Event{Type: watch.Error, Object: &metav1.Status{
		Status: metav1.StatusFailure, Code: 500, Reason: metav1.StatusReasonInternalError, Message: "etcd unavailable",
	}}
	wc := &scriptedWatch{current: bpRev("50", 5), streams: [][]watch.Event{
		{internal},
		{{Type: watch.Modified, Object: bpRev("51", 7)}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	decided, gap, err := watchRevisions(ctx, wc, client.ObjectKey{Namespace: "ns", Name: "node.n1"}, "10",
		func(u *unstructured.Unstructured) bool { return u.GetGeneration() == 7 })
	if !apierrors.IsInternalError(err) {
		t.Fatalf("err = %v, want the streamed 500", err)
	}
	if decided || gap || wc.gets != 0 {
		t.Errorf("decided=%v gap=%v reads=%d, want false/false/0: a 500 is not an expired history", decided, gap, wc.gets)
	}
}
