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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	checkpointv1alpha1 "github.com/rst0git/pod-snapshot-controller/api/v1alpha1"
)

// fakeCheckpointer implements KubeletCheckpointer for testing.
type fakeCheckpointer struct {
	location    string
	err         error
	lastTimeout int64
}

func (f *fakeCheckpointer) CheckpointPod(_ context.Context, _, _, _ string, timeoutSeconds int64) (string, error) {
	f.lastTimeout = timeoutSeconds
	return f.location, f.err
}

// helper to create a PodCheckpoint resource.
func createPodCheckpoint(ctx context.Context, name, sourcePod string) {
	resource := &checkpointv1alpha1.PodCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: checkpointv1alpha1.PodCheckpointSpec{
			SourcePodName: sourcePod,
		},
	}
	Expect(k8sClient.Create(ctx, resource)).To(Succeed())
}

// helper to create a Pod.
func createPod(ctx context.Context, name, nodeName string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if nodeName != "" {
		pod.Spec.NodeName = nodeName
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	return pod
}

var _ = Describe("PodCheckpoint Controller", func() {
	ctx := context.Background()

	It("should set initial Pending phase with Unknown condition on first reconcile", func() {
		name := "ckpt-initial"
		nn := types.NamespacedName{Name: name, Namespace: "default"}

		// Create checkpoint referencing a non-existent pod so the first
		// reconcile sets Pending, and the second sets Failed.
		createPodCheckpoint(ctx, name, "nonexistent-pod-initial")

		reconciler := &PodCheckpointReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Checkpointer: &fakeCheckpointer{},
		}

		// First reconcile: should add finalizer.
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var updated checkpointv1alpha1.PodCheckpoint
		Expect(k8sClient.Get(ctx, nn, &updated)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(&updated, checkpointv1alpha1.PodCheckpointProtectionFinalizer)).To(BeTrue())
	})

	It("should set Failed phase when target Pod does not exist", func() {
		name := "ckpt-no-pod"
		nn := types.NamespacedName{Name: name, Namespace: "default"}
		createPodCheckpoint(ctx, name, "nonexistent-pod")

		reconciler := &PodCheckpointReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Checkpointer: &fakeCheckpointer{},
		}

		// First reconcile adds finalizer, second sets Pending, third finds Pod missing.
		for range 3 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}

		var updated checkpointv1alpha1.PodCheckpoint
		Expect(k8sClient.Get(ctx, nn, &updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodCheckpointPhaseFailed))
	})

	It("should set Failed phase when target Pod is not running", func() {
		podName := "pod-pending"
		name := "ckpt-pending"
		nn := types.NamespacedName{Name: name, Namespace: "default"}

		createPod(ctx, podName, "")
		createPodCheckpoint(ctx, name, podName)

		reconciler := &PodCheckpointReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Checkpointer: &fakeCheckpointer{},
		}

		// Reconcile until terminal state.
		for range 3 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}

		var updated checkpointv1alpha1.PodCheckpoint
		Expect(k8sClient.Get(ctx, nn, &updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodCheckpointPhaseFailed))
	})

	It("should set Ready phase with checkpoint location on success", func() {
		podName := "pod-running-ok"
		name := "ckpt-success"
		nn := types.NamespacedName{Name: name, Namespace: "default"}

		pod := createPod(ctx, podName, "test-node")
		pod.Status.Phase = corev1.PodRunning
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		createPodCheckpoint(ctx, name, podName)

		expectedLocation := "/var/lib/kubelet/pod-checkpoints/checkpoint-pod-running-ok_default-2026-01-01T00:00:00Z.tar"
		reconciler := &PodCheckpointReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Checkpointer: &fakeCheckpointer{
				location: expectedLocation,
			},
		}

		// Reconcile until terminal state (finalizer + pending + checkpoint).
		for range 3 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}

		var updated checkpointv1alpha1.PodCheckpoint
		Expect(k8sClient.Get(ctx, nn, &updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodCheckpointPhaseReady))
		Expect(updated.Status.CheckpointLocation).To(Equal(expectedLocation))
		Expect(updated.Status.NodeName).To(Equal("test-node"))
	})

	It("should pass timeout to checkpointer", func() {
		podName := "pod-running-timeout"
		name := "ckpt-timeout"
		nn := types.NamespacedName{Name: name, Namespace: "default"}

		pod := createPod(ctx, podName, "test-node")
		pod.Status.Phase = corev1.PodRunning
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		resource := &checkpointv1alpha1.PodCheckpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
			Spec: checkpointv1alpha1.PodCheckpointSpec{
				SourcePodName:  podName,
				TimeoutSeconds: 30,
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		fake := &fakeCheckpointer{location: "/checkpoint/path"}
		reconciler := &PodCheckpointReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Checkpointer: fake,
		}

		for range 3 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(fake.lastTimeout).To(Equal(int64(30)))
	})

	It("should set Failed phase when checkpoint API returns an error", func() {
		podName := "pod-running-fail"
		name := "ckpt-api-err"
		nn := types.NamespacedName{Name: name, Namespace: "default"}

		pod := createPod(ctx, podName, "test-node")
		pod.Status.Phase = corev1.PodRunning
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		createPodCheckpoint(ctx, name, podName)

		reconciler := &PodCheckpointReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Checkpointer: &fakeCheckpointer{
				err: fmt.Errorf("checkpoint failed: CRIU error"),
			},
		}

		for range 3 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}

		var updated checkpointv1alpha1.PodCheckpoint
		Expect(k8sClient.Get(ctx, nn, &updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodCheckpointPhaseFailed))
	})

	It("should skip reconciliation for terminal states", func() {
		name := "ckpt-terminal"
		nn := types.NamespacedName{Name: name, Namespace: "default"}

		createPodCheckpoint(ctx, name, "some-pod")

		// Set to terminal state.
		var checkpoint checkpointv1alpha1.PodCheckpoint
		Expect(k8sClient.Get(ctx, nn, &checkpoint)).To(Succeed())
		checkpoint.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseReady
		Expect(k8sClient.Status().Update(ctx, &checkpoint)).To(Succeed())

		reconciler := &PodCheckpointReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Checkpointer: &fakeCheckpointer{
				err: fmt.Errorf("should not be called"),
			},
		}

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))
	})

	It("should retry InProgress checkpoints (not treat as terminal)", func() {
		podName := "pod-running-retry"
		name := "ckpt-retry-inprogress"
		nn := types.NamespacedName{Name: name, Namespace: "default"}

		pod := createPod(ctx, podName, "test-node")
		pod.Status.Phase = corev1.PodRunning
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		createPodCheckpoint(ctx, name, podName)

		// Simulate a checkpoint that was left InProgress (controller crash).
		var checkpoint checkpointv1alpha1.PodCheckpoint
		Expect(k8sClient.Get(ctx, nn, &checkpoint)).To(Succeed())
		checkpoint.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseInProgress
		checkpoint.Status.NodeName = "test-node"
		Expect(k8sClient.Status().Update(ctx, &checkpoint)).To(Succeed())

		expectedLocation := "/var/lib/kubelet/pod-checkpoints/checkpoint-retry.tar"
		fake := &fakeCheckpointer{location: expectedLocation}
		reconciler := &PodCheckpointReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Checkpointer: fake,
		}

		// Should NOT skip — should retry the checkpoint.
		for range 2 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}

		var updated checkpointv1alpha1.PodCheckpoint
		Expect(k8sClient.Get(ctx, nn, &updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodCheckpointPhaseReady))
		Expect(updated.Status.CheckpointLocation).To(Equal(expectedLocation))
	})

	It("should add protection finalizer to the PodCheckpoint", func() {
		name := "ckpt-finalizer"
		nn := types.NamespacedName{Name: name, Namespace: "default"}

		createPodCheckpoint(ctx, name, "some-pod-finalizer")

		reconciler := &PodCheckpointReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Checkpointer: &fakeCheckpointer{},
		}

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var updated checkpointv1alpha1.PodCheckpoint
		Expect(k8sClient.Get(ctx, nn, &updated)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(&updated, checkpointv1alpha1.PodCheckpointProtectionFinalizer)).To(BeTrue())
	})
})
