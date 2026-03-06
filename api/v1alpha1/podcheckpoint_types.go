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

// PodCheckpoint phase constants.
const (
	PodCheckpointPhasePending    = "Pending"
	PodCheckpointPhaseInProgress = "InProgress"
	PodCheckpointPhaseReady      = "Ready"
	PodCheckpointPhaseFailed     = "Failed"
)

// Condition type constants.
const (
	ConditionReady = "Ready"
)

// PodCheckpointSpec defines the desired state of PodCheckpoint.
type PodCheckpointSpec struct {
	// sourcePodName is the name of the running Pod to checkpoint.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SourcePodName string `json:"sourcePodName"`

	// timeoutSeconds is the timeout for the checkpoint operation in seconds.
	// 0 means use the container runtime default.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TimeoutSeconds int64 `json:"timeoutSeconds,omitempty"`
}

// CheckpointContainerInfo stores the name and image of a checkpointed container
// so that the restore controller can create a Pod with matching container specs.
type CheckpointContainerInfo struct {
	// name is the Kubernetes container name.
	Name string `json:"name"`
	// image is the container image.
	Image string `json:"image"`
}

// PodCheckpointStatus defines the observed state of PodCheckpoint.
type PodCheckpointStatus struct {
	// phase represents the current phase: Pending, InProgress, Ready, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// conditions represent the latest available observations.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// nodeName is the node where the source Pod was running when checkpointed.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// checkpointLocation is the path to the checkpoint data on the node.
	// +optional
	CheckpointLocation string `json:"checkpointLocation,omitempty"`

	// containers lists the containers that were checkpointed.
	// +optional
	Containers []CheckpointContainerInfo `json:"containers,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Source Pod",type=string,JSONPath=".spec.sourcePodName"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".status.nodeName"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// PodCheckpoint is the Schema for the podcheckpoints API.
// It represents a checkpoint (snapshot) of all containers in a Pod.
type PodCheckpoint struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PodCheckpoint.
	// +required
	Spec PodCheckpointSpec `json:"spec"`

	// status defines the observed state of PodCheckpoint.
	// +optional
	Status PodCheckpointStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PodCheckpointList contains a list of PodCheckpoint.
type PodCheckpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PodCheckpoint `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodCheckpoint{}, &PodCheckpointList{})
}
