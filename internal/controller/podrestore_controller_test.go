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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
			restoreName    = "test-restore"
			checkpointName = "test-checkpoint"
			namespace      = "default"
			nodeName       = "test-node"
			checkpointPath = "/var/lib/kubelet/pod-checkpoints/checkpoint-myPod_default-2026-03-09T12:00:00Z.tar"
		)

		ctx := context.Background()

		restoreNN := types.NamespacedName{Name: restoreName, Namespace: namespace}
		checkpointNN := types.NamespacedName{Name: checkpointName, Namespace: namespace}

		createCheckpoint := func(phase string) {
			ckpt := &checkpointv1alpha1.PodCheckpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name:      checkpointName,
					Namespace: namespace,
				},
				Spec: checkpointv1alpha1.PodCheckpointSpec{
					SourcePodName: "myPod",
				},
			}
			err := k8sClient.Get(ctx, checkpointNN, ckpt)
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, ckpt)).To(Succeed())
			}
			// Update status.
			Expect(k8sClient.Get(ctx, checkpointNN, ckpt)).To(Succeed())
			ckpt.Status.Phase = phase
			ckpt.Status.NodeName = nodeName
			ckpt.Status.CheckpointLocation = checkpointPath
			Expect(k8sClient.Status().Update(ctx, ckpt)).To(Succeed())
		}

		createRestore := func() {
			restore := &checkpointv1alpha1.PodRestore{}
			err := k8sClient.Get(ctx, restoreNN, restore)
			if err != nil && errors.IsNotFound(err) {
				resource := &checkpointv1alpha1.PodRestore{
					ObjectMeta: metav1.ObjectMeta{
						Name:      restoreName,
						Namespace: namespace,
					},
					Spec: checkpointv1alpha1.PodRestoreSpec{
						CheckpointName: checkpointName,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		}

		cleanup := func() {
			restore := &checkpointv1alpha1.PodRestore{}
			if err := k8sClient.Get(ctx, restoreNN, restore); err == nil {
				Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
			}
			ckpt := &checkpointv1alpha1.PodCheckpoint{}
			if err := k8sClient.Get(ctx, checkpointNN, ckpt); err == nil {
				Expect(k8sClient.Delete(ctx, ckpt)).To(Succeed())
			}
		}

		AfterEach(func() {
			cleanup()
		})

		It("should set Failed phase when referenced PodCheckpoint does not exist", func() {
			resource := &checkpointv1alpha1.PodRestore{
				ObjectMeta: metav1.ObjectMeta{
					Name:      restoreName,
					Namespace: namespace,
				},
				Spec: checkpointv1alpha1.PodRestoreSpec{
					CheckpointName: "nonexistent-checkpoint",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			r := &PodRestoreReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Restorer: &fakeRestorer{podSandboxID: "fake-id"},
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
			Expect(err).NotTo(HaveOccurred())

			var updated checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodRestorePhaseFailed))
		})

		It("should requeue when PodCheckpoint is not ready", func() {
			createCheckpoint(checkpointv1alpha1.PodCheckpointPhaseInProgress)
			createRestore()

			r := &PodRestoreReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Restorer: &fakeRestorer{podSandboxID: "fake-id"},
			}

			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		})

		It("should call kubelet restore API and set Completed on success", func() {
			createCheckpoint(checkpointv1alpha1.PodCheckpointPhaseReady)
			createRestore()

			fake := &fakeRestorer{podSandboxID: "restored-sandbox-123"}
			r := &PodRestoreReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Restorer: fake,
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
			Expect(err).NotTo(HaveOccurred())

			// Verify the restorer was called with correct parameters.
			Expect(fake.lastNode).To(Equal(nodeName))
			Expect(fake.lastNS).To(Equal(namespace))
			Expect(fake.lastCkptName).To(Equal("checkpoint-myPod_default-2026-03-09T12:00:00Z.tar"))

			var updated checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodRestorePhaseCompleted))
			Expect(updated.Status.RestoredSandboxID).To(Equal("restored-sandbox-123"))
		})

		It("should set Failed phase when kubelet restore API fails", func() {
			createCheckpoint(checkpointv1alpha1.PodCheckpointPhaseReady)
			createRestore()

			fake := &fakeRestorer{err: fmt.Errorf("restore failed: container runtime error")}
			r := &PodRestoreReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Restorer: fake,
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
			Expect(err).NotTo(HaveOccurred())

			var updated checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(checkpointv1alpha1.PodRestorePhaseFailed))
		})

		It("should not reconcile when already completed", func() {
			createCheckpoint(checkpointv1alpha1.PodCheckpointPhaseReady)
			createRestore()

			// Manually set status to Completed.
			var restore checkpointv1alpha1.PodRestore
			Expect(k8sClient.Get(ctx, restoreNN, &restore)).To(Succeed())
			restore.Status.Phase = checkpointv1alpha1.PodRestorePhaseCompleted
			Expect(k8sClient.Status().Update(ctx, &restore)).To(Succeed())

			fake := &fakeRestorer{podSandboxID: "should-not-be-called"}
			r := &PodRestoreReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Restorer: fake,
			}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: restoreNN})
			Expect(err).NotTo(HaveOccurred())

			// Restorer should not have been called.
			Expect(fake.lastNode).To(BeEmpty())
		})
	})
})
