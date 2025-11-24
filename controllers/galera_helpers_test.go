/*
Copyright 2023.

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

package controllers

import (
	"testing"

	"k8s.io/utils/ptr"

	mariadbv1 "github.com/openstack-k8s-operators/mariadb-operator/api/v1beta1"
)

func TestFindBestCandidate_AllReplicasPresent(t *testing.T) {
	galera := &mariadbv1.Galera{
		Spec: mariadbv1.GaleraSpec{
			GaleraSpecCore: mariadbv1.GaleraSpecCore{
				Replicas: ptr.To[int32](3),
			},
		},
		Status: mariadbv1.GaleraStatus{
			Attributes: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {Seqno: "100"},
				"galera-1": {Seqno: "150"},
				"galera-2": {Seqno: "120"},
			},
		},
	}

	node, found := findBestCandidate(galera)
	if !found {
		t.Fatal("Expected to find a candidate")
	}
	if node != "galera-1" {
		t.Errorf("Expected galera-1 (highest seqno), got %s", node)
	}
}

func TestFindBestCandidate_SafeToBootstrapTakesPriority(t *testing.T) {
	galera := &mariadbv1.Galera{
		Spec: mariadbv1.GaleraSpec{
			GaleraSpecCore: mariadbv1.GaleraSpecCore{
				Replicas: ptr.To[int32](3),
			},
		},
		Status: mariadbv1.GaleraStatus{
			Attributes: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {Seqno: "100"},
				"galera-1": {Seqno: "150"},
				"galera-2": {Seqno: "120", SafeToBootstrap: true},
			},
		},
	}

	node, found := findBestCandidate(galera)
	if !found {
		t.Fatal("Expected to find a candidate")
	}
	if node != "galera-2" {
		t.Errorf("Expected galera-2 (SafeToBootstrap), got %s", node)
	}
}

func TestFindBestCandidate_BootstrapRetryAfterFailure(t *testing.T) {
	// This tests the fix: when bootstrap fails, the pod's runtime state (Gcomm, ContainerID)
	// is cleared but the persistent data (Seqno, UUID) is preserved by clearPodRuntimeState.
	// This allows us to retry bootstrap while preventing data loss.
	galera := &mariadbv1.Galera{
		Spec: mariadbv1.GaleraSpec{
			GaleraSpecCore: mariadbv1.GaleraSpecCore{
				Replicas: ptr.To[int32](3),
			},
		},
		Status: mariadbv1.GaleraStatus{
			Attributes: map[string]mariadbv1.GaleraAttributes{
				// All 3 pods have been probed. galera-2 has the highest seqno.
				// galera-2 was selected for bootstrap but failed.
				// clearPodRuntimeState cleared Gcomm/ContainerID but kept the seqno.
				"galera-0": {Seqno: "100"},
				"galera-1": {Seqno: "150"},
				"galera-2": {Seqno: "200"}, // Highest seqno, runtime state cleared after failure
			},
		},
	}

	node, found := findBestCandidate(galera)
	if !found {
		t.Fatal("Expected to find a candidate")
	}
	// Should still pick galera-2 because we preserved its seqno
	if node != "galera-2" {
		t.Errorf("Expected galera-2 (highest seqno preserved after failure), got %s", node)
	}
}

func TestFindBestCandidate_SinglePodWithAttributes(t *testing.T) {
	// Edge case: only one pod has been probed so far (others still starting)
	// Should wait for all pods to be probed before making a decision
	galera := &mariadbv1.Galera{
		Spec: mariadbv1.GaleraSpec{
			GaleraSpecCore: mariadbv1.GaleraSpecCore{
				Replicas: ptr.To[int32](3),
			},
		},
		Status: mariadbv1.GaleraStatus{
			Attributes: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {Seqno: "0"},
			},
		},
	}

	node, found := findBestCandidate(galera)
	if found {
		t.Errorf("Expected to wait for all pods, but got candidate %s", node)
	}
}

func TestFindBestCandidate_NoAttributes(t *testing.T) {
	// When no pods have attributes yet (initial state), should not find candidate
	galera := &mariadbv1.Galera{
		Spec: mariadbv1.GaleraSpec{
			GaleraSpecCore: mariadbv1.GaleraSpecCore{
				Replicas: ptr.To[int32](3),
			},
		},
		Status: mariadbv1.GaleraStatus{
			Attributes: map[string]mariadbv1.GaleraAttributes{},
		},
	}

	node, found := findBestCandidate(galera)
	if found {
		t.Errorf("Expected no candidate when no attributes present, but got %s", node)
	}
}

func TestFindBestCandidate_SingleNodeCluster(t *testing.T) {
	galera := &mariadbv1.Galera{
		Spec: mariadbv1.GaleraSpec{
			GaleraSpecCore: mariadbv1.GaleraSpecCore{
				Replicas: ptr.To[int32](1),
			},
		},
		Status: mariadbv1.GaleraStatus{
			Attributes: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {Seqno: "0"},
			},
		},
	}

	node, found := findBestCandidate(galera)
	if !found {
		t.Fatal("Expected to find a candidate")
	}
	if node != "galera-0" {
		t.Errorf("Expected galera-0, got %s", node)
	}
}

func TestFindBestCandidate_EqualSeqno(t *testing.T) {
	galera := &mariadbv1.Galera{
		Spec: mariadbv1.GaleraSpec{
			GaleraSpecCore: mariadbv1.GaleraSpecCore{
				Replicas: ptr.To[int32](3),
			},
		},
		Status: mariadbv1.GaleraStatus{
			Attributes: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {Seqno: "100"},
				"galera-1": {Seqno: "100"},
				"galera-2": {Seqno: "100"},
			},
		},
	}

	node, found := findBestCandidate(galera)
	if !found {
		t.Fatal("Expected to find a candidate")
	}
	// Due to sort order and >= comparison, should pick the last one
	if node != "galera-2" {
		t.Errorf("Expected galera-2 (last in sorted order), got %s", node)
	}
}

func TestBuildGcommURI(t *testing.T) {
	tests := []struct {
		name      string
		replicas  int32
		namespace string
		expected  string
	}{
		{
			name:      "3-node cluster",
			replicas:  3,
			namespace: "openstack",
			expected:  "gcomm://galera-galera-0.galera-galera.openstack.svc,galera-galera-1.galera-galera.openstack.svc,galera-galera-2.galera-galera.openstack.svc",
		},
		{
			name:      "1-node cluster",
			replicas:  1,
			namespace: "openstack",
			expected:  "gcomm://galera-galera-0.galera-galera.openstack.svc",
		},
		{
			name:      "5-node cluster",
			replicas:  5,
			namespace: "test-ns",
			expected:  "gcomm://galera-galera-0.galera-galera.test-ns.svc,galera-galera-1.galera-galera.test-ns.svc,galera-galera-2.galera-galera.test-ns.svc,galera-galera-3.galera-galera.test-ns.svc,galera-galera-4.galera-galera.test-ns.svc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			galera := &mariadbv1.Galera{
				Spec: mariadbv1.GaleraSpec{
					GaleraSpecCore: mariadbv1.GaleraSpecCore{
						Replicas: ptr.To(tt.replicas),
					},
				},
			}
			galera.Name = "galera"
			galera.Namespace = tt.namespace

			uri := buildGcommURI(galera)
			if uri != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, uri)
			}
		})
	}
}

func TestIsBootstrapInProgress(t *testing.T) {
	tests := []struct {
		name       string
		attributes map[string]mariadbv1.GaleraAttributes
		expected   bool
	}{
		{
			name: "bootstrap in progress",
			attributes: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {Gcomm: "gcomm://"},
			},
			expected: true,
		},
		{
			name: "nodes joining cluster",
			attributes: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {Gcomm: "gcomm://galera-0.galera,galera-1.galera"},
				"galera-1": {Gcomm: "gcomm://galera-0.galera,galera-1.galera"},
			},
			expected: false,
		},
		{
			name:       "no gcomm URI",
			attributes: map[string]mariadbv1.GaleraAttributes{},
			expected:   false,
		},
		{
			name: "attributes cleared after bootstrap failure",
			attributes: map[string]mariadbv1.GaleraAttributes{
				"galera-1": {Seqno: "0"},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			galera := &mariadbv1.Galera{
				Status: mariadbv1.GaleraStatus{
					Attributes: tt.attributes,
				},
			}

			result := isBootstrapInProgress(galera)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestClearPodRuntimeState(t *testing.T) {
	tests := []struct {
		name          string
		initialAttrs  map[string]mariadbv1.GaleraAttributes
		podName       string
		expectedAttrs map[string]mariadbv1.GaleraAttributes
	}{
		{
			name: "clears runtime state but preserves persistent data",
			initialAttrs: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {
					UUID:            "uuid-123",
					Seqno:           "200",
					SafeToBootstrap: true,
					NoGrastate:      false,
					Gcomm:           "gcomm://",
					ContainerID:     "container-abc",
				},
			},
			podName: "galera-0",
			expectedAttrs: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {
					UUID:            "uuid-123",
					Seqno:           "200",
					SafeToBootstrap: true,
					NoGrastate:      false,
					Gcomm:           "", // cleared
					ContainerID:     "", // cleared
				},
			},
		},
		{
			name: "handles non-existent pod gracefully",
			initialAttrs: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {Seqno: "100"},
			},
			podName: "galera-1",
			expectedAttrs: map[string]mariadbv1.GaleraAttributes{
				"galera-0": {Seqno: "100"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			galera := &mariadbv1.Galera{
				Status: mariadbv1.GaleraStatus{
					Attributes: tt.initialAttrs,
				},
			}

			clearPodRuntimeState(galera, tt.podName)

			// Compare attributes
			if len(galera.Status.Attributes) != len(tt.expectedAttrs) {
				t.Errorf("Expected %d attributes, got %d", len(tt.expectedAttrs), len(galera.Status.Attributes))
			}

			for podName, expectedAttr := range tt.expectedAttrs {
				actualAttr, found := galera.Status.Attributes[podName]
				if !found {
					t.Errorf("Expected pod %s in attributes", podName)
					continue
				}
				if actualAttr.UUID != expectedAttr.UUID {
					t.Errorf("Pod %s: Expected UUID %s, got %s", podName, expectedAttr.UUID, actualAttr.UUID)
				}
				if actualAttr.Seqno != expectedAttr.Seqno {
					t.Errorf("Pod %s: Expected Seqno %s, got %s", podName, expectedAttr.Seqno, actualAttr.Seqno)
				}
				if actualAttr.SafeToBootstrap != expectedAttr.SafeToBootstrap {
					t.Errorf("Pod %s: Expected SafeToBootstrap %v, got %v", podName, expectedAttr.SafeToBootstrap, actualAttr.SafeToBootstrap)
				}
				if actualAttr.Gcomm != expectedAttr.Gcomm {
					t.Errorf("Pod %s: Expected Gcomm %s, got %s", podName, expectedAttr.Gcomm, actualAttr.Gcomm)
				}
				if actualAttr.ContainerID != expectedAttr.ContainerID {
					t.Errorf("Pod %s: Expected ContainerID %s, got %s", podName, expectedAttr.ContainerID, actualAttr.ContainerID)
				}
			}
		})
	}
}
