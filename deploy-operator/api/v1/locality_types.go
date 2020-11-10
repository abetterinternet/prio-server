/*
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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocalitySpec defines the desired state of Locality
type LocalitySpec struct {
	// EnvironmentName is the name of the environment for prio
	EnvironmentName string `json:"environmentName"`

	// ManifestBucketLocation is the location that the manifest buckets are stored
	ManifestBucketLocation string `json:"manifestBucketLocation"`

	// DataShareProcessors is the list of data share processors
	DataShareProcessors []string `json:"dataShareProcessors"`

	// Schedule is the cron job schedule as defined by https://en.wikipedia.org/wiki/Cron
	Schedule string `json:"schedule"`
}

// LocalityStatus defines the observed state of Locality
type LocalityStatus struct {
	// LastKeyRotationJob is the last time a key rotation job ran and updated this Resource
	LastKeyRotationJob *metav1.Time `json:"lastKeyRotationRun,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Locality is the Schema for the localities API
type Locality struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LocalitySpec   `json:"spec,omitempty"`
	Status LocalityStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LocalityList contains a list of Locality
type LocalityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Locality `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Locality{}, &LocalityList{})
}