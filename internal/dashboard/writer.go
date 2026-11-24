// Copyright (C) 2026 Intel Corporation
// SPDX-License-Identifier: Apache-2.0

package dashboard

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cpopv1alpha1 "github.com/intel/cpu-pinning-operator/api/v1alpha1"
	"github.com/intel/cpu-pinning-operator/internal/topology"
)

// Config controls where and how per-node dashboards are written (chart grafanaDashboard.*).
type Config struct {
	// Enabled gates dashboard writing; PruneNode runs regardless.
	Enabled bool
	// Namespace for the dashboard ConfigMaps; it must match what the Grafana sidecar watches.
	Namespace string
	// SidecarLabel and SidecarLabelValue are what the Grafana sidecar selects on.
	SidecarLabel      string
	SidecarLabelValue string
	// FolderAnnotation and Folder set the Grafana folder; an empty annotation means General.
	FolderAnnotation string
	Folder           string
	// InstanceName is the release name, used to name and label the ConfigMaps.
	InstanceName string
	// DatasourceUID pins the Prometheus datasource variable; empty auto-selects.
	DatasourceUID string
	// Loki adds the machine's log section; zero omits it.
	Loki LokiConfig
}

// Writer creates/updates and deletes per-node dashboard ConfigMaps.
type Writer struct {
	client.Client
	Config Config
}

const (
	// managedByLabel and managedByValue tag the ConfigMaps this writer owns, for pruning.
	managedByLabel = cpopv1alpha1.LabelPrefix + "dashboard"
	managedByValue = "node-canvas"
	// nodeLabel records which node a dashboard ConfigMap belongs to.
	nodeLabel = cpopv1alpha1.LabelPrefix + "node"
	dataKey   = "cpu-pinning-canvas.json"
)

// DashboardUID returns the Grafana UID for a node's dashboard: prefix + node, as
// the fleet dashboard's links rebuild it.
// Grafana caps UIDs at 40 chars, leaving 27 for the node name; hashing
// the overflow needs each UID published as a metric label for the fleet links.
func DashboardUID(node string) string {
	return "cpu-pin-node-" + node
}

// configMapName is the per-node ConfigMap name; DNS-1123 node names fit with the prefix.
func (w *Writer) configMapName(node string) string {
	return w.Config.InstanceName + "-canvas-" + node
}

// Write renders and applies one node's dashboard ConfigMap; a no-op when disabled
// or before the topology is cached.
func (w *Writer) Write(ctx context.Context, node string, cpus map[int]topology.CPUInfo, machine Machine) error {
	if !w.Config.Enabled {
		return nil
	}
	if len(cpus) == 0 {
		return nil
	}
	ds := Datasources{PrometheusUID: w.Config.DatasourceUID, Loki: w.Config.Loki}
	data, err := RenderDashboardJSON(node, ds, machine, cpus)
	if err != nil {
		return fmt.Errorf("render dashboard for node %s: %w", node, err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      w.configMapName(node),
			Namespace: w.Config.Namespace,
		},
	}
	labels := map[string]string{
		managedByLabel: managedByValue,
		nodeLabel:      node,
	}
	if w.Config.SidecarLabel != "" {
		labels[w.Config.SidecarLabel] = w.Config.SidecarLabelValue
	}
	annotations := map[string]string{}
	if w.Config.FolderAnnotation != "" {
		annotations[w.Config.FolderAnnotation] = w.Config.Folder
	}

	existing := &corev1.ConfigMap{}
	err = w.Get(ctx, client.ObjectKey{Name: cm.Name, Namespace: cm.Namespace}, existing)
	if errors.IsNotFound(err) {
		cm.Labels = labels
		if len(annotations) > 0 {
			cm.Annotations = annotations
		}
		cm.Data = map[string]string{dataKey: string(data)}
		return w.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	// Merge our labels/annotations onto whatever is there, then set data.
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for k, v := range labels {
		existing.Labels[k] = v
	}
	if len(annotations) > 0 {
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		for k, v := range annotations {
			existing.Annotations[k] = v
		}
	}
	existing.Data = map[string]string{dataKey: string(data)}
	return w.Update(ctx, existing)
}

// PruneNode deletes a node's dashboard ConfigMap, regardless of Enabled.
func (w *Writer) PruneNode(ctx context.Context, node string) error {
	if w.Config.Namespace == "" {
		return nil
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      w.configMapName(node),
			Namespace: w.Config.Namespace,
		},
	}
	err := w.Delete(ctx, cm)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
