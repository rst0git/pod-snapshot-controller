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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	checkpointv1alpha1 "github.com/rst0git/pod-snapshot-controller/api/v1alpha1"
)

// KubeletCheckpointer abstracts the kubelet checkpoint API call so it can
// be replaced in tests.
type KubeletCheckpointer interface {
	CheckpointPod(ctx context.Context, nodeName, namespace, podName string, timeoutSeconds int64) (string, error)
}

// PodCheckpointReconciler reconciles a PodCheckpoint object by checkpointing
// all containers in the target Pod via the kubelet checkpoint API.
type PodCheckpointReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	KubeClient   kubernetes.Interface
	Checkpointer KubeletCheckpointer
}

// +kubebuilder:rbac:groups=checkpoint.k8s.io,resources=podcheckpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=checkpoint.k8s.io,resources=podcheckpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=checkpoint.k8s.io,resources=podcheckpoints/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes/proxy,verbs=create

// Reconcile handles checkpoint requests by calling the kubelet pod-level
// checkpoint API via the API server's node proxy endpoint.
func (r *PodCheckpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var checkpoint checkpointv1alpha1.PodCheckpoint
	if err := r.Get(ctx, req.NamespacedName, &checkpoint); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion: clean up checkpoint data and remove finalizer.
	if !checkpoint.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &checkpoint)
	}

	// Add protection finalizer if not present.
	if !controllerutil.ContainsFinalizer(&checkpoint, checkpointv1alpha1.PodCheckpointProtectionFinalizer) {
		controllerutil.AddFinalizer(&checkpoint, checkpointv1alpha1.PodCheckpointProtectionFinalizer)
		if err := r.Update(ctx, &checkpoint); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch after metadata update.
		if err := r.Get(ctx, req.NamespacedName, &checkpoint); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Skip terminal states only (Ready and Failed).
	// InProgress is NOT terminal — if the controller crashed mid-operation,
	// we must retry. The kubelet checkpoint API is idempotent.
	if checkpoint.Status.Phase == checkpointv1alpha1.PodCheckpointPhaseReady ||
		checkpoint.Status.Phase == checkpointv1alpha1.PodCheckpointPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Set initial Pending phase and condition if not yet set.
	if checkpoint.Status.Phase == "" {
		checkpoint.Status.Phase = checkpointv1alpha1.PodCheckpointPhasePending
		meta.SetStatusCondition(&checkpoint.Status.Conditions, metav1.Condition{
			Type:               checkpointv1alpha1.ConditionReady,
			Status:             metav1.ConditionUnknown,
			Reason:             "Pending",
			Message:            "Checkpoint request accepted, awaiting processing",
			ObservedGeneration: checkpoint.Generation,
		})
		if err := r.Status().Update(ctx, &checkpoint); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, req.NamespacedName, &checkpoint); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Look up the target Pod.
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: checkpoint.Namespace, Name: checkpoint.Spec.SourcePodName}, &pod); err != nil {
		log.Error(err, "Failed to get target Pod", "pod", checkpoint.Spec.SourcePodName)
		return ctrl.Result{}, r.setFailed(ctx, &checkpoint,
			fmt.Sprintf("Pod %q not found: %v", checkpoint.Spec.SourcePodName, err))
	}

	if pod.Status.Phase != corev1.PodRunning {
		return ctrl.Result{}, r.setFailed(ctx, &checkpoint,
			fmt.Sprintf("Pod %q is not running (phase: %s)", pod.Name, pod.Status.Phase))
	}

	if pod.Spec.NodeName == "" {
		return ctrl.Result{}, r.setFailed(ctx, &checkpoint,
			fmt.Sprintf("Pod %q has no node assigned", pod.Name))
	}

	// Add source Pod protection finalizer to prevent deletion during checkpoint.
	if err := r.addSourcePodFinalizer(ctx, &pod); err != nil {
		return ctrl.Result{}, err
	}

	// Set phase to InProgress.
	checkpoint.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseInProgress
	checkpoint.Status.NodeName = pod.Spec.NodeName
	meta.SetStatusCondition(&checkpoint.Status.Conditions, metav1.Condition{
		Type:               checkpointv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "CheckpointInProgress",
		Message:            fmt.Sprintf("Checkpointing Pod %q on node %q", pod.Name, pod.Spec.NodeName),
		ObservedGeneration: checkpoint.Generation,
	})
	if err := r.Status().Update(ctx, &checkpoint); err != nil {
		return ctrl.Result{}, err
	}

	// Re-fetch after status update to get the latest resource version.
	if err := r.Get(ctx, req.NamespacedName, &checkpoint); err != nil {
		return ctrl.Result{}, err
	}

	// Call pod-level checkpoint via the kubelet API (through API server node proxy).
	location, err := r.Checkpointer.CheckpointPod(ctx, pod.Spec.NodeName, checkpoint.Namespace, checkpoint.Spec.SourcePodName, checkpoint.Spec.TimeoutSeconds)
	if err != nil {
		log.Error(err, "Pod checkpoint failed", "pod", checkpoint.Spec.SourcePodName)
		// Remove source Pod protection on failure.
		if removeErr := r.removeSourcePodFinalizer(ctx, checkpoint.Namespace, checkpoint.Spec.SourcePodName); removeErr != nil {
			log.Error(removeErr, "Failed to remove source pod finalizer after checkpoint failure")
		}
		return ctrl.Result{}, r.setFailed(ctx, &checkpoint, fmt.Sprintf("Checkpoint failed: %v", err))
	}

	log.Info("Pod checkpointed successfully", "pod", checkpoint.Spec.SourcePodName, "location", location)

	// Remove source Pod protection finalizer — checkpoint is complete.
	if removeErr := r.removeSourcePodFinalizer(ctx, checkpoint.Namespace, checkpoint.Spec.SourcePodName); removeErr != nil {
		log.Error(removeErr, "Failed to remove source pod finalizer after checkpoint success")
	}

	checkpoint.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseReady
	checkpoint.Status.CheckpointLocation = location

	// Store container info so the restore controller can create a Pod
	// with matching container specs.
	var containers []checkpointv1alpha1.CheckpointContainerInfo
	for _, c := range pod.Spec.Containers {
		containers = append(containers, checkpointv1alpha1.CheckpointContainerInfo{
			Name:  c.Name,
			Image: c.Image,
		})
	}
	checkpoint.Status.Containers = containers
	meta.SetStatusCondition(&checkpoint.Status.Conditions, metav1.Condition{
		Type:               checkpointv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "CheckpointCompleted",
		Message:            fmt.Sprintf("Pod %q checkpointed successfully", checkpoint.Spec.SourcePodName),
		ObservedGeneration: checkpoint.Generation,
	})

	return ctrl.Result{}, r.Status().Update(ctx, &checkpoint)
}

// handleDeletion processes PodCheckpoint deletion by checking for active
// restores, optionally cleaning up checkpoint data, and removing the finalizer.
func (r *PodCheckpointReconciler) handleDeletion(ctx context.Context, checkpoint *checkpointv1alpha1.PodCheckpoint) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(checkpoint, checkpointv1alpha1.PodCheckpointProtectionFinalizer) {
		return ctrl.Result{}, nil
	}

	// Check if any PodRestore references this checkpoint.
	var restoreList checkpointv1alpha1.PodRestoreList
	if err := r.List(ctx, &restoreList, client.InNamespace(checkpoint.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for _, restore := range restoreList.Items {
		if restore.Spec.CheckpointName == checkpoint.Name &&
			restore.Status.Phase != checkpointv1alpha1.PodRestorePhaseCompleted &&
			restore.Status.Phase != checkpointv1alpha1.PodRestorePhaseFailed {
			log.Info("PodCheckpoint is referenced by an active PodRestore, deferring deletion",
				"restore", restore.Name)
			return ctrl.Result{RequeueAfter: 5 * 1e9}, nil // 5 seconds
		}
	}

	// Clean up source pod finalizer if it's still present (e.g., controller
	// crashed during checkpoint).
	if checkpoint.Status.Phase == checkpointv1alpha1.PodCheckpointPhaseInProgress {
		if removeErr := r.removeSourcePodFinalizer(ctx, checkpoint.Namespace, checkpoint.Spec.SourcePodName); removeErr != nil {
			log.Error(removeErr, "Failed to remove source pod finalizer during deletion")
		}
	}

	// TODO: If deletionPolicy is Delete and checkpoint data exists on the
	// node, call a kubelet endpoint to clean up checkpoint files.
	// For alpha, log the intent — actual node-side cleanup requires a
	// kubelet deletion endpoint that does not yet exist.
	if checkpoint.Spec.DeletionPolicy == checkpointv1alpha1.DeletionPolicyDelete &&
		checkpoint.Status.CheckpointLocation != "" {
		log.Info("Checkpoint data should be cleaned up on node",
			"node", checkpoint.Status.NodeName,
			"location", checkpoint.Status.CheckpointLocation)
	}

	controllerutil.RemoveFinalizer(checkpoint, checkpointv1alpha1.PodCheckpointProtectionFinalizer)
	return ctrl.Result{}, r.Update(ctx, checkpoint)
}

// addSourcePodFinalizer adds a protection finalizer to the source Pod to
// prevent accidental deletion while a checkpoint operation is in progress.
func (r *PodCheckpointReconciler) addSourcePodFinalizer(ctx context.Context, pod *corev1.Pod) error {
	if controllerutil.ContainsFinalizer(pod, checkpointv1alpha1.SourcePodProtectionFinalizer) {
		return nil
	}
	controllerutil.AddFinalizer(pod, checkpointv1alpha1.SourcePodProtectionFinalizer)
	return r.Update(ctx, pod)
}

// removeSourcePodFinalizer removes the protection finalizer from the source Pod.
func (r *PodCheckpointReconciler) removeSourcePodFinalizer(ctx context.Context, namespace, podName string) error {
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, &pod); err != nil {
		if errors.IsNotFound(err) {
			return nil // Pod already gone.
		}
		return err
	}
	if !controllerutil.ContainsFinalizer(&pod, checkpointv1alpha1.SourcePodProtectionFinalizer) {
		return nil
	}
	controllerutil.RemoveFinalizer(&pod, checkpointv1alpha1.SourcePodProtectionFinalizer)
	return r.Update(ctx, &pod)
}

// checkpointResponse represents the JSON response from the kubelet
// pod-level checkpoint API: {"items":["<checkpoint_path>"]}.
type checkpointResponse struct {
	Items []string `json:"items"`
}

// KubeletCheckpointClient implements KubeletCheckpointer by calling the
// kubelet pod-level checkpoint API via the API server's node proxy endpoint.
type KubeletCheckpointClient struct {
	KubeClient kubernetes.Interface
}

// CheckpointPod calls POST /api/v1/nodes/{node}/proxy/checkpoint/{namespace}/{pod}
// and parses the JSON response to extract the checkpoint path.
func (c *KubeletCheckpointClient) CheckpointPod(
	ctx context.Context, nodeName, namespace, podName string, timeoutSeconds int64,
) (string, error) {
	req := c.KubeClient.CoreV1().RESTClient().Post().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy", "checkpoint", namespace, podName)

	if timeoutSeconds > 0 {
		req = req.Param("timeout", fmt.Sprintf("%d", timeoutSeconds))
	}

	res := req.Do(ctx)

	if err := res.Error(); err != nil {
		return "", fmt.Errorf("checkpoint failed: %w", err)
	}

	body, err := res.Raw()
	if err != nil {
		return "", fmt.Errorf("failed to read checkpoint response: %w", err)
	}

	var resp checkpointResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("failed to parse checkpoint response: %w", err)
	}

	if len(resp.Items) == 0 {
		return "", fmt.Errorf("checkpoint response contained no items")
	}

	return resp.Items[0], nil
}

// setFailed updates the PodCheckpoint status to Failed with a condition message.
func (r *PodCheckpointReconciler) setFailed(
	ctx context.Context, checkpoint *checkpointv1alpha1.PodCheckpoint, message string,
) error {
	checkpoint.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseFailed
	meta.SetStatusCondition(&checkpoint.Status.Conditions, metav1.Condition{
		Type:               checkpointv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "CheckpointFailed",
		Message:            message,
		ObservedGeneration: checkpoint.Generation,
	})
	return r.Status().Update(ctx, checkpoint)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodCheckpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&checkpointv1alpha1.PodCheckpoint{}).
		Named("podcheckpoint").
		Complete(r)
}
