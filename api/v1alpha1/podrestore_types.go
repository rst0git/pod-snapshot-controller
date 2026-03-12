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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodRestore phase constants.
const (
	PodRestorePhasePending   = "Pending"
	PodRestorePhaseRestoring = "Restoring"
	PodRestorePhaseCompleted = "Completed"
	PodRestorePhaseFailed    = "Failed"
)

// PodRestoreSpec defines the desired state of PodRestore.
type PodRestoreSpec struct {
	// checkpointName references the PodCheckpoint to restore from.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="checkpointName is immutable"
	CheckpointName string `json:"checkpointName"`
}

// PodRestoreStatus defines the observed state of PodRestore.
type PodRestoreStatus struct {
	// phase: Pending, Restoring, Completed, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// conditions represent the latest available observations.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// restoredSandboxID is the CRI sandbox ID of the restored pod.
	// +optional
	RestoredSandboxID string `json:"restoredSandboxId,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Checkpoint",type=string,JSONPath=".spec.checkpointName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Sandbox ID",type=string,JSONPath=".status.restoredSandboxId"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// PodRestore is the Schema for the podrestores API.
// It represents a restore operation from a PodCheckpoint.
type PodRestore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PodRestore.
	// +required
	Spec PodRestoreSpec `json:"spec"`

	// status defines the observed state of PodRestore.
	// +optional
	Status PodRestoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PodRestoreList contains a list of PodRestore.
type PodRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PodRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodRestore{}, &PodRestoreList{})
}
