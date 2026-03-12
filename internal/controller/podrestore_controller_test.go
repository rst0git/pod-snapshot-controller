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
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	checkpointv1alpha1 "github.com/rst0git/pod-snapshot-controller/api/v1alpha1"
)

// fakeRestorer implements KubeletRestorer for testing.
type fakeRestorer struct {
	podSandboxID string
	err          error
	lastNode     string
	lastNS       string
	lastCkptName string
}

func (f *fakeRestorer) RestorePod(_ context.Context, nodeName, namespace, checkpointName string) (string, error) {
	f.lastNode = nodeName
	f.lastNS = namespace
	f.lastCkptName = checkpointName
	return f.podSandboxID, f.err
}

var _ = Describe("PodRestore Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			namespace = "default"
			nodeName  = "test-node"
		)

		ctx := context.Background()

		It("should set initial Pending phase with Unknown condition", func() {
			restoreName := "restore-pending"
			ckptName := "ckpt-for-pending"
			restoreNN := types.NamespacedName{Name: restoreName, Namespace: namespace}
			checkpointNN := types.NamespacedName{Name: ckptName, Namespace: namespace}

			ckpt := &checkpointv1alpha1.PodCheckpoint{
				ObjectMeta: metav1.ObjectMeta{Name: ckptName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodCheckpointSpec{SourcePodName: "some-pod"},
			}
			Expect(k8sClient.Create(ctx, ckpt)).To(Succeed())
			Expect(k8sClient.Get(ctx, checkpointNN, ckpt)).To(Succeed())
			ckpt.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseInProgress
			ckpt.Status.NodeName = nodeName
			ckpt.Status.CheckpointLocation = "/some/path"
			Expect(k8sClient.Status().Update(ctx, ckpt)).To(Succeed())

			restore := &checkpointv1alpha1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodRestoreSpec{CheckpointName: ckptName},
			}
			Expect(k8sClient.Create(ctx, restore)).To(Succeed())

			r := &PodRestoreReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Restorer: &fakeRestorer{podSandboxID: "fake-id"},
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
			Expect(err).NotTo(HaveOccurred())

			var updated checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodRestorePhasePending))

			// Cleanup.
			Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ckpt)).To(Succeed())
		})

		It("should set Failed phase when referenced PodCheckpoint does not exist", func() {
			restoreName := "restore-no-ckpt"
			restoreNN := types.NamespacedName{Name: restoreName, Namespace: namespace}

			resource := &checkpointv1alpha1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodRestoreSpec{CheckpointName: "nonexistent-checkpoint"},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			r := &PodRestoreReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Restorer: &fakeRestorer{podSandboxID: "fake-id"},
			}

			// First reconcile sets Pending, second finds checkpoint missing.
			for range 2 {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
				Expect(err).NotTo(HaveOccurred())
			}

			var updated checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodRestorePhaseFailed))

			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should requeue when PodCheckpoint is not ready", func() {
			restoreName := "restore-requeue"
			ckptName := "ckpt-for-requeue"
			restoreNN := types.NamespacedName{Name: restoreName, Namespace: namespace}
			checkpointNN := types.NamespacedName{Name: ckptName, Namespace: namespace}

			ckpt := &checkpointv1alpha1.PodCheckpoint{
				ObjectMeta: metav1.ObjectMeta{Name: ckptName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodCheckpointSpec{SourcePodName: "some-pod"},
			}
			Expect(k8sClient.Create(ctx, ckpt)).To(Succeed())
			Expect(k8sClient.Get(ctx, checkpointNN, ckpt)).To(Succeed())
			ckpt.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseInProgress
			ckpt.Status.NodeName = nodeName
			ckpt.Status.CheckpointLocation = "/some/path"
			Expect(k8sClient.Status().Update(ctx, ckpt)).To(Succeed())

			restore := &checkpointv1alpha1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodRestoreSpec{CheckpointName: ckptName},
			}
			Expect(k8sClient.Create(ctx, restore)).To(Succeed())

			r := &PodRestoreReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Restorer: &fakeRestorer{podSandboxID: "fake-id"},
			}

			// First reconcile sets Pending, second requeues.
			var result reconcile.Result
			for range 2 {
				var err error
				result, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ckpt)).To(Succeed())
		})

		It("should call kubelet restore API and set Completed on success", func() {
			restoreName := "restore-success"
			ckptName := "ckpt-for-success"
			sourcePod := "pod-for-success"
			ckptPath := "/var/lib/kubelet/pod-snapshots/snapshot-pod-for-success_default-2026-03-09T12:00:00Z"
			restoreNN := types.NamespacedName{Name: restoreName, Namespace: namespace}
			checkpointNN := types.NamespacedName{Name: ckptName, Namespace: namespace}

			ckpt := &checkpointv1alpha1.PodCheckpoint{
				ObjectMeta: metav1.ObjectMeta{Name: ckptName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodCheckpointSpec{SourcePodName: sourcePod},
			}
			Expect(k8sClient.Create(ctx, ckpt)).To(Succeed())
			Expect(k8sClient.Get(ctx, checkpointNN, ckpt)).To(Succeed())
			ckpt.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseReady
			ckpt.Status.NodeName = nodeName
			ckpt.Status.CheckpointLocation = ckptPath
			Expect(k8sClient.Status().Update(ctx, ckpt)).To(Succeed())

			restore := &checkpointv1alpha1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodRestoreSpec{CheckpointName: ckptName},
			}
			Expect(k8sClient.Create(ctx, restore)).To(Succeed())

			// Pre-create the placeholder Pod with an IP to simulate sandbox ready.
			fakeKubeClient := fake.NewClientset()
			placeholderPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: sourcePod, Namespace: namespace},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
					NodeName:   nodeName,
				},
				Status: corev1.PodStatus{PodIP: "10.0.0.1"},
			}
			_, err := fakeKubeClient.CoreV1().Pods(namespace).Create(ctx, placeholderPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Also create in envtest so the controller can see its status.
			envtestPod := placeholderPod.DeepCopy()
			envtestPod.ResourceVersion = ""
			Expect(k8sClient.Create(ctx, envtestPod)).To(Succeed())
			envtestPod.Status.PodIP = "10.0.0.1"
			Expect(k8sClient.Status().Update(ctx, envtestPod)).To(Succeed())

			fakeRest := &fakeRestorer{podSandboxID: "restored-sandbox-123"}
			r := &PodRestoreReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				Restorer:   fakeRest,
				KubeClient: fakeKubeClient,
			}

			// Reconcile: Pending -> Restoring -> check sandbox -> Completed
			for range 3 {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
				Expect(err).NotTo(HaveOccurred())
			}

			Expect(fakeRest.lastNode).To(Equal(nodeName))
			Expect(fakeRest.lastNS).To(Equal(namespace))
			Expect(fakeRest.lastCkptName).To(Equal("snapshot-pod-for-success_default-2026-03-09T12:00:00Z"))

			var updated checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodRestorePhaseCompleted))
			Expect(updated.Status.RestoredSandboxID).To(Equal("restored-sandbox-123"))

			// Cleanup.
			Expect(k8sClient.Delete(ctx, envtestPod)).To(Succeed())
			Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ckpt)).To(Succeed())
		})

		It("should set Failed phase when kubelet restore API fails", func() {
			restoreName := "restore-fail"
			ckptName := "ckpt-for-fail"
			sourcePod := "pod-for-fail"
			ckptPath := "/var/lib/kubelet/pod-snapshots/snapshot-pod-for-fail_default-2026-03-09T12:00:00Z"
			restoreNN := types.NamespacedName{Name: restoreName, Namespace: namespace}
			checkpointNN := types.NamespacedName{Name: ckptName, Namespace: namespace}

			ckpt := &checkpointv1alpha1.PodCheckpoint{
				ObjectMeta: metav1.ObjectMeta{Name: ckptName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodCheckpointSpec{SourcePodName: sourcePod},
			}
			Expect(k8sClient.Create(ctx, ckpt)).To(Succeed())
			Expect(k8sClient.Get(ctx, checkpointNN, ckpt)).To(Succeed())
			ckpt.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseReady
			ckpt.Status.NodeName = nodeName
			ckpt.Status.CheckpointLocation = ckptPath
			Expect(k8sClient.Status().Update(ctx, ckpt)).To(Succeed())

			restore := &checkpointv1alpha1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodRestoreSpec{CheckpointName: ckptName},
			}
			Expect(k8sClient.Create(ctx, restore)).To(Succeed())

			fakeKubeClient := fake.NewClientset()
			placeholderPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: sourcePod, Namespace: namespace},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
					NodeName:   nodeName,
				},
				Status: corev1.PodStatus{PodIP: "10.0.0.1"},
			}
			_, err := fakeKubeClient.CoreV1().Pods(namespace).Create(ctx, placeholderPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			envtestPod := placeholderPod.DeepCopy()
			envtestPod.ResourceVersion = ""
			Expect(k8sClient.Create(ctx, envtestPod)).To(Succeed())
			envtestPod.Status.PodIP = "10.0.0.1"
			Expect(k8sClient.Status().Update(ctx, envtestPod)).To(Succeed())

			r := &PodRestoreReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				Restorer:   &fakeRestorer{err: fmt.Errorf("restore failed: container runtime error")},
				KubeClient: fakeKubeClient,
			}

			for range 3 {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
				Expect(err).NotTo(HaveOccurred())
			}

			var updated checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodRestorePhaseFailed))

			Expect(k8sClient.Delete(ctx, envtestPod)).To(Succeed())
			Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ckpt)).To(Succeed())
		})

		It("should not reconcile when already completed", func() {
			restoreName := "restore-completed"
			ckptName := "ckpt-for-completed"
			restoreNN := types.NamespacedName{Name: restoreName, Namespace: namespace}
			checkpointNN := types.NamespacedName{Name: ckptName, Namespace: namespace}

			ckpt := &checkpointv1alpha1.PodCheckpoint{
				ObjectMeta: metav1.ObjectMeta{Name: ckptName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodCheckpointSpec{SourcePodName: "some-pod"},
			}
			Expect(k8sClient.Create(ctx, ckpt)).To(Succeed())
			Expect(k8sClient.Get(ctx, checkpointNN, ckpt)).To(Succeed())
			ckpt.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseReady
			ckpt.Status.NodeName = nodeName
			ckpt.Status.CheckpointLocation = "/some/path"
			Expect(k8sClient.Status().Update(ctx, ckpt)).To(Succeed())

			restore := &checkpointv1alpha1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodRestoreSpec{CheckpointName: ckptName},
			}
			Expect(k8sClient.Create(ctx, restore)).To(Succeed())

			// Manually set status to Completed.
			var restoreObj checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &restoreObj)).To(Succeed())
			restoreObj.Status.Phase = checkpointv1alpha1.PodRestorePhaseCompleted
			Expect(k8sClient.Status().Update(ctx, &restoreObj)).To(Succeed())

			fakeRest := &fakeRestorer{podSandboxID: "should-not-be-called"}
			r := &PodRestoreReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Restorer: fakeRest,
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeRest.lastNode).To(BeEmpty())

			Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ckpt)).To(Succeed())
		})

		It("should retry Restoring phase (not treat as terminal)", func() {
			restoreName := "restore-retry"
			ckptName := "ckpt-for-retry"
			sourcePod := "pod-for-retry"
			ckptPath := "/var/lib/kubelet/pod-snapshots/snapshot-pod-for-retry_default-2026-03-09T12:00:00Z"
			restoreNN := types.NamespacedName{Name: restoreName, Namespace: namespace}
			checkpointNN := types.NamespacedName{Name: ckptName, Namespace: namespace}

			ckpt := &checkpointv1alpha1.PodCheckpoint{
				ObjectMeta: metav1.ObjectMeta{Name: ckptName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodCheckpointSpec{SourcePodName: sourcePod},
			}
			Expect(k8sClient.Create(ctx, ckpt)).To(Succeed())
			Expect(k8sClient.Get(ctx, checkpointNN, ckpt)).To(Succeed())
			ckpt.Status.Phase = checkpointv1alpha1.PodCheckpointPhaseReady
			ckpt.Status.NodeName = nodeName
			ckpt.Status.CheckpointLocation = ckptPath
			Expect(k8sClient.Status().Update(ctx, ckpt)).To(Succeed())

			restore := &checkpointv1alpha1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: namespace},
				Spec:       checkpointv1alpha1.PodRestoreSpec{CheckpointName: ckptName},
			}
			Expect(k8sClient.Create(ctx, restore)).To(Succeed())

			// Simulate a restore left in Restoring (controller crash).
			var restoreObj checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &restoreObj)).To(Succeed())
			restoreObj.Status.Phase = checkpointv1alpha1.PodRestorePhaseRestoring
			Expect(k8sClient.Status().Update(ctx, &restoreObj)).To(Succeed())

			fakeKubeClient := fake.NewClientset()
			placeholderPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: sourcePod, Namespace: namespace},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
					NodeName:   nodeName,
				},
				Status: corev1.PodStatus{PodIP: "10.0.0.1"},
			}
			_, err := fakeKubeClient.CoreV1().Pods(namespace).Create(ctx, placeholderPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			envtestPod := placeholderPod.DeepCopy()
			envtestPod.ResourceVersion = ""
			Expect(k8sClient.Create(ctx, envtestPod)).To(Succeed())
			envtestPod.Status.PodIP = "10.0.0.1"
			Expect(k8sClient.Status().Update(ctx, envtestPod)).To(Succeed())

			fakeRest := &fakeRestorer{podSandboxID: "retried-sandbox-456"}
			r := &PodRestoreReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				Restorer:   fakeRest,
				KubeClient: fakeKubeClient,
			}

			// Should NOT skip — should retry the restore.
			for range 2 {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
				Expect(err).NotTo(HaveOccurred())
			}

			var updated checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodRestorePhaseCompleted))
			Expect(updated.Status.RestoredSandboxID).To(Equal("retried-sandbox-456"))

			Expect(k8sClient.Delete(ctx, envtestPod)).To(Succeed())
			Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ckpt)).To(Succeed())
		})
	})
})
