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
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
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

	// Skip if already in progress or in a terminal state.
	if checkpoint.Status.Phase == checkpointv1alpha1.PodCheckpointPhaseInProgress ||
		checkpoint.Status.Phase == checkpointv1alpha1.PodCheckpointPhaseReady ||
		checkpoint.Status.Phase == checkpointv1alpha1.PodCheckpointPhaseFailed {
		return ctrl.Result{}, nil
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

	// Set phase to InProgress.
	checkpoint.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseInProgress
	checkpoint.Status.NodeName = pod.Spec.NodeName
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
		checkpoint.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseFailed
		meta.SetStatusCondition(&checkpoint.Status.Conditions, metav1.Condition{
			Type:               checkpointv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "CheckpointFailed",
			Message:            err.Error(),
			ObservedGeneration: checkpoint.Generation,
		})
		return ctrl.Result{}, r.Status().Update(ctx, &checkpoint)
	}

	log.Info("Pod checkpointed successfully", "pod", checkpoint.Spec.SourcePodName, "location", location)

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
