/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	checkpointv1alpha1 "github.com/rst0git/pod-snapshot-controller/api/v1alpha1"
)

// KubeletRestorer abstracts the kubelet restore API call so it can
// be replaced in tests.
type KubeletRestorer interface {
	RestorePod(ctx context.Context, nodeName, namespace, checkpointName string) (string, error)
}

// PodRestoreReconciler reconciles a PodRestore object by calling the
// kubelet restore API to restore a pod from a completed PodCheckpoint.
type PodRestoreReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Restorer   KubeletRestorer
	KubeClient kubernetes.Interface
}

// +kubebuilder:rbac:groups=checkpoint.k8s.io,resources=podrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=checkpoint.k8s.io,resources=podrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=checkpoint.k8s.io,resources=podrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=checkpoint.k8s.io,resources=podcheckpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/proxy,verbs=create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;create;delete

// Reconcile handles restore requests by reading the referenced PodCheckpoint
// and calling the kubelet restore API via the API server's node proxy endpoint.
func (r *PodRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var restore checkpointv1alpha1.PodRestore
	if err := r.Get(ctx, req.NamespacedName, &restore); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Skip terminal states only (Completed and Failed).
	// Restoring is NOT terminal — if the controller crashed mid-operation,
	// we must retry.
	if restore.Status.Phase == checkpointv1alpha1.PodRestorePhaseCompleted ||
		restore.Status.Phase == checkpointv1alpha1.PodRestorePhaseFailed {
		return ctrl.Result{}, nil
	}

	// Set initial Pending phase and condition if not yet set.
	if restore.Status.Phase == "" {
		restore.Status.Phase = checkpointv1alpha1.PodRestorePhasePending
		meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
			Type:               checkpointv1alpha1.ConditionReady,
			Status:             metav1.ConditionUnknown,
			Reason:             "Pending",
			Message:            "Restore request accepted, awaiting processing",
			ObservedGeneration: restore.Generation,
		})
		if err := r.Status().Update(ctx, &restore); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, req.NamespacedName, &restore); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Get the referenced PodCheckpoint.
	var checkpoint checkpointv1alpha1.PodCheckpoint
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: restore.Namespace,
		Name:      restore.Spec.CheckpointName,
	}, &checkpoint); err != nil {
		log.Error(err, "Failed to get PodCheckpoint", "checkpoint", restore.Spec.CheckpointName)
		return ctrl.Result{}, r.setFailed(ctx, &restore,
			fmt.Sprintf("PodCheckpoint %q not found: %v", restore.Spec.CheckpointName, err))
	}

	// Wait for the checkpoint to be ready.
	if checkpoint.Status.Phase != checkpointv1alpha1.PodCheckpointPhaseReady {
		log.Info("PodCheckpoint not yet ready, requeueing",
			"checkpoint", checkpoint.Name, "phase", checkpoint.Status.Phase)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if checkpoint.Status.NodeName == "" {
		return ctrl.Result{}, r.setFailed(ctx, &restore,
			fmt.Sprintf("PodCheckpoint %q has no node name", checkpoint.Name))
	}

	if checkpoint.Status.CheckpointLocation == "" {
		return ctrl.Result{}, r.setFailed(ctx, &restore,
			fmt.Sprintf("PodCheckpoint %q has no checkpoint location", checkpoint.Name))
	}

	// Set phase to Restoring.
	restore.Status.Phase = checkpointv1alpha1.PodRestorePhaseRestoring
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               checkpointv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "RestoreInProgress",
		Message:            fmt.Sprintf("Restoring from checkpoint %q on node %q", checkpoint.Name, checkpoint.Status.NodeName),
		ObservedGeneration: restore.Generation,
	})
	if err := r.Status().Update(ctx, &restore); err != nil {
		return ctrl.Result{}, err
	}

	// Re-fetch after status update.
	if err := r.Get(ctx, req.NamespacedName, &restore); err != nil {
		return ctrl.Result{}, err
	}

	// Extract the checkpoint filename from the full path.
	// The kubelet restore API expects the basename as the checkpoint name.
	checkpointFileName := filepath.Base(checkpoint.Status.CheckpointLocation)

	// Create a Pod object before calling the kubelet restore API.
	// The kubelet cannot create pods (Node authorization restricts it
	// to mirror pods), but CNI plugins like Calico require the Pod to
	// exist in the API server when setting up networking.
	podName := checkpoint.Spec.SourcePodName
	if err := r.ensurePodForRestore(ctx, podName, restore.Namespace, checkpoint.Status.NodeName, checkpoint.Name, checkpoint.Status.Containers); err != nil {
		log.Error(err, "Failed to create Pod for restore")
		return ctrl.Result{}, r.setFailed(ctx, &restore,
			fmt.Sprintf("Failed to create Pod: %v", err))
	}

	// Wait for the placeholder Pod to have a sandbox set up by the kubelet.
	// Instead of a fixed sleep, poll until the Pod is observed as Running
	// or has container statuses (indicating kubelet has synced it), or
	// requeue if not yet ready.
	podReady, err := r.isPodSandboxReady(ctx, podName, restore.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !podReady {
		log.Info("Waiting for kubelet to set up placeholder Pod sandbox", "pod", podName)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Call the kubelet restore API via the API server node proxy.
	podSandboxID, err := r.Restorer.RestorePod(
		ctx,
		checkpoint.Status.NodeName,
		restore.Namespace,
		checkpointFileName,
	)
	if err != nil {
		log.Error(err, "Pod restore failed",
			"checkpoint", checkpoint.Name,
			"node", checkpoint.Status.NodeName)
		return ctrl.Result{}, r.setFailed(ctx, &restore,
			fmt.Sprintf("Restore failed: %v", err))
	}

	log.Info("Pod restored successfully",
		"checkpoint", checkpoint.Name,
		"podSandboxId", podSandboxID,
		"node", checkpoint.Status.NodeName)

	// Re-fetch before final status update to avoid conflicts.
	if err := r.Get(ctx, req.NamespacedName, &restore); err != nil {
		return ctrl.Result{}, err
	}

	restore.Status.Phase = checkpointv1alpha1.PodRestorePhaseCompleted
	restore.Status.RestoredSandboxID = podSandboxID
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               checkpointv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "RestoreCompleted",
		Message:            fmt.Sprintf("Pod restored from checkpoint %q (sandbox: %s)", restore.Spec.CheckpointName, podSandboxID),
		ObservedGeneration: restore.Generation,
	})

	return ctrl.Result{}, r.Status().Update(ctx, &restore)
}

// isPodSandboxReady checks whether the kubelet has synced the placeholder Pod
// by looking for a PodIP or container statuses, which indicate the sandbox
// has been created with networking.
func (r *PodRestoreReconciler) isPodSandboxReady(ctx context.Context, podName, namespace string) (bool, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, &pod); err != nil {
		return false, err
	}

	// If the Pod has an IP assigned, the sandbox is up with networking.
	if pod.Status.PodIP != "" {
		return true, nil
	}

	// If any container status is reported, the kubelet has synced.
	if len(pod.Status.ContainerStatuses) > 0 {
		return true, nil
	}

	return false, nil
}

// restoreResponse represents the JSON response from the kubelet
// pod-level restore API: {"podSandboxId":"<id>"}.
type restoreResponse struct {
	PodSandboxID string `json:"podSandboxId"`
}

// KubeletRestoreClient implements KubeletRestorer by calling the
// kubelet pod-level restore API via the API server's node proxy endpoint.
type KubeletRestoreClient struct {
	KubeClient kubernetes.Interface
}

// RestorePod calls POST /api/v1/nodes/{node}/proxy/restore/{namespace}/{checkpointName}
// and parses the JSON response to extract the restored pod sandbox ID.
func (c *KubeletRestoreClient) RestorePod(
	ctx context.Context, nodeName, namespace, checkpointName string,
) (string, error) {
	res := c.KubeClient.CoreV1().RESTClient().Post().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy", "restore", namespace, checkpointName).
		Do(ctx)

	if err := res.Error(); err != nil {
		return "", fmt.Errorf("restore failed: %w", err)
	}

	body, err := res.Raw()
	if err != nil {
		return "", fmt.Errorf("failed to read restore response: %w", err)
	}

	var resp restoreResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("failed to parse restore response: %w", err)
	}

	if resp.PodSandboxID == "" {
		return "", fmt.Errorf("restore response contained no pod sandbox ID")
	}

	return resp.PodSandboxID, nil
}

// ensurePodForRestore creates a placeholder Pod in the API server so that
// CNI plugins (e.g. Calico) can look it up during network setup when the
// kubelet's CRI RestorePod call creates a new sandbox. The kubelet's
// RestorePod will read this Pod's UID and use it for the sandbox config.
func (r *PodRestoreReconciler) ensurePodForRestore(
	ctx context.Context, podName, namespace, nodeName, checkpointName string,
	containerInfos []checkpointv1alpha1.CheckpointContainerInfo,
) error {
	log := logf.FromContext(ctx)

	// Check if the Pod already exists (e.g. from a previous attempt).
	_, err := r.KubeClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err == nil {
		log.Info("Pod already exists, reusing for restore", "pod", podName)
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check existing pod: %w", err)
	}

	// Build container specs from the checkpoint's stored container info.
	// Using the actual container names and images ensures the kubelet's
	// sync loop recognizes the restored containers as matching the Pod spec.
	var containers []corev1.Container
	if len(containerInfos) > 0 {
		for _, ci := range containerInfos {
			containers = append(containers, corev1.Container{
				Name:  ci.Name,
				Image: ci.Image,
			})
		}
	} else {
		// Fallback if checkpoint doesn't have container info.
		containers = []corev1.Container{
			{
				Name:  "restore-placeholder",
				Image: "registry.k8s.io/pause:3.10",
			},
		}
	}

	// Create the Pod with spec.nodeName set so that:
	// 1. The kubelet starts the Pod (creating a sandbox with networking)
	// 2. CNI plugins can look up the Pod by name during network setup
	// The kubelet may start the containers normally before RestorePod is
	// called, but containerd's RestorePod will detect the existing sandbox
	// and reuse it, stopping any running containers before CRIU restore.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Annotations: map[string]string{
				"checkpoint.k8s.io/restored-from": checkpointName,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: containers,
		},
	}

	createdPod, err := r.KubeClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			log.Info("Pod was created concurrently, continuing", "pod", podName)
			return nil
		}
		return fmt.Errorf("failed to create pod: %w", err)
	}

	log.Info("Created Pod for restore",
		"pod", podName, "uid", createdPod.UID, "node", nodeName)

	return nil
}

// setFailed updates the PodRestore status to Failed with a condition message.
func (r *PodRestoreReconciler) setFailed(
	ctx context.Context, restore *checkpointv1alpha1.PodRestore, message string,
) error {
	restore.Status.Phase = checkpointv1alpha1.PodRestorePhaseFailed
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               checkpointv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "RestoreFailed",
		Message:            message,
		ObservedGeneration: restore.Generation,
	})
	return r.Status().Update(ctx, restore)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&checkpointv1alpha1.PodRestore{}).
		Named("podrestore").
		Complete(r)
}
