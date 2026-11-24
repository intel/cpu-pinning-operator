// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// ClientsetLogsGetter implements LogsGetter using a client-go clientset,
// since controller-runtime's client.Client does not expose the pod logs subresource.
type ClientsetLogsGetter struct {
	Clientset kubernetes.Interface
}

// GetLogs returns the combined stdout/stderr log output of the named pod's
// (single) container.
func (g *ClientsetLogsGetter) GetLogs(ctx context.Context, namespace, podName string) (string, error) {
	req := g.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
