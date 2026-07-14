package controller

import (
	"testing"

	mariadbv1 "github.com/openstack-k8s-operators/mariadb-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func int32Ptr(i int32) *int32 { return &i }

func makeGalera(replicas int32, attrs map[string]mariadbv1.GaleraAttributes) *mariadbv1.Galera {
	return makeGaleraNamed("test-ns", "galera", replicas, attrs)
}

func makeGaleraNamed(ns, name string, replicas int32, attrs map[string]mariadbv1.GaleraAttributes) *mariadbv1.Galera {
	return &mariadbv1.Galera{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: mariadbv1.GaleraSpec{
			GaleraSpecCore: mariadbv1.GaleraSpecCore{
				Replicas: int32Ptr(replicas),
			},
		},
		Status: mariadbv1.GaleraStatus{
			Attributes: attrs,
		},
	}
}

func makePod(name, containerID string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "galera", ContainerID: containerID},
			},
		},
	}
}

func makePodWithPhase(name, containerID string, phase corev1.PodPhase) corev1.Pod {
	pod := makePod(name, containerID)
	pod.Status.Phase = phase
	return pod
}

func TestFindBestCandidate_AllFreshCIDs(t *testing.T) {
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "100", ContainerID: "cid-0"},
		"galera-1": {Seqno: "100", ContainerID: "cid-1"},
		"galera-2": {Seqno: "101", ContainerID: "cid-2"},
	})
	pods := []corev1.Pod{
		makePod("galera-0", "cid-0"),
		makePod("galera-1", "cid-1"),
		makePod("galera-2", "cid-2"),
	}
	node, found := findBestCandidate(g, pods, ctrl.Log)
	if !found {
		t.Fatal("expected to find a candidate")
	}
	if node != "galera-2" {
		t.Errorf("expected galera-2 (highest seqno), got %s", node)
	}
}

func TestFindBestCandidate_SafeToBootstrap(t *testing.T) {
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "100", ContainerID: "cid-0", SafeToBootstrap: true},
		"galera-1": {Seqno: "200", ContainerID: "cid-1"},
		"galera-2": {Seqno: "200", ContainerID: "cid-2"},
	})
	pods := []corev1.Pod{
		makePod("galera-0", "cid-0"),
		makePod("galera-1", "cid-1"),
		makePod("galera-2", "cid-2"),
	}
	node, found := findBestCandidate(g, pods, ctrl.Log)
	if !found {
		t.Fatal("expected to find a candidate")
	}
	if node != "galera-0" {
		t.Errorf("expected galera-0 (SafeToBootstrap), got %s", node)
	}
}

func TestFindBestCandidate_StaleCIDs_StillWorks(t *testing.T) {
	// Pods have restarted and have new CIDs, but attributes still have
	// old CIDs from a previous push. findBestCandidate should NOT care
	// about CID freshness -- it only needs seqno data from all replicas.
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "100", ContainerID: "old-cid-0"},
		"galera-1": {Seqno: "100", ContainerID: "old-cid-1"},
		"galera-2": {Seqno: "100", ContainerID: "old-cid-2"},
	})
	pods := []corev1.Pod{
		makePod("galera-0", "new-cid-0"),
		makePod("galera-1", "new-cid-1"),
		makePod("galera-2", "new-cid-2"),
	}
	node, found := findBestCandidate(g, pods, ctrl.Log)
	if !found {
		t.Fatal("expected to find a candidate even with stale CIDs")
	}
	t.Logf("Selected node: %s", node)
}

func TestFindBestCandidate_NotAllReported(t *testing.T) {
	// Only 2 of 3 replicas have pushed attributes
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "100", ContainerID: "cid-0"},
		"galera-1": {Seqno: "100", ContainerID: "cid-1"},
	})
	pods := []corev1.Pod{
		makePod("galera-0", "cid-0"),
		makePod("galera-1", "cid-1"),
		makePod("galera-2", "cid-2"),
	}
	_, found := findBestCandidate(g, pods, ctrl.Log)
	if found {
		t.Error("should not find candidate when not all replicas have reported")
	}
}

func TestFindBestCandidate_UnequalSeqno(t *testing.T) {
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "100", ContainerID: "cid-0"},
		"galera-1": {Seqno: "200", ContainerID: "cid-1"},
		"galera-2": {Seqno: "150", ContainerID: "cid-2"},
	})
	pods := []corev1.Pod{
		makePod("galera-0", "cid-0"),
		makePod("galera-1", "cid-1"),
		makePod("galera-2", "cid-2"),
	}
	node, found := findBestCandidate(g, pods, ctrl.Log)
	if !found {
		t.Fatal("expected to find a candidate")
	}
	if node != "galera-1" {
		t.Errorf("expected galera-1 (highest seqno=200), got %s", node)
	}
}

// Reproduces the exact scenario from the live failure
func TestFindBestCandidate_LiveScenario_AllEqualSeqno(t *testing.T) {
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"openstack-cell1-galera-0": {
			UUID:        "918a5168-7773-11f1-bce9-4ad1e2e8f877",
			Seqno:       "1892",
			ContainerID: "cri-o://4c72f596",
		},
		"openstack-cell1-galera-1": {
			UUID:        "918a5168-7773-11f1-bce9-4ad1e2e8f877",
			Seqno:       "1892",
			ContainerID: "cri-o://90a8fb82",
		},
		"openstack-cell1-galera-2": {
			UUID:        "918a5168-7773-11f1-bce9-4ad1e2e8f877",
			Seqno:       "1892",
			ContainerID: "cri-o://7cd39428",
		},
	})
	// Pods have DIFFERENT CIDs (restarted since pushing)
	pods := []corev1.Pod{
		makePod("openstack-cell1-galera-0", "cri-o://NEW-0"),
		makePod("openstack-cell1-galera-1", "cri-o://NEW-1"),
		makePod("openstack-cell1-galera-2", "cri-o://NEW-2"),
	}
	node, found := findBestCandidate(g, pods, ctrl.Log)
	if !found {
		t.Fatal("expected to find a candidate despite CID mismatches")
	}
	t.Logf("Selected node: %s", node)
}

func TestIsBootstrapInProgress_NoState(t *testing.T) {
	r := &GaleraReconciler{}
	g := makeGaleraNamed("ns", "galera", 3, nil)
	pods := []corev1.Pod{makePod("galera-0", "cid-0")}
	if r.isBootstrapInProgress(g, pods) {
		t.Error("expected false when no bootstrap state exists")
	}
}

func TestIsBootstrapInProgress_SameCID(t *testing.T) {
	r := &GaleraReconciler{}
	g := makeGaleraNamed("ns", "galera", 3, nil)
	r.setBootstrapInProgress(g, "galera-0", "cid-0")

	pods := []corev1.Pod{makePod("galera-0", "cid-0")}
	if !r.isBootstrapInProgress(g, pods) {
		t.Error("expected true when bootstrap pod is still running with same CID")
	}
}

func TestIsBootstrapInProgress_PodRestarted(t *testing.T) {
	r := &GaleraReconciler{}
	g := makeGaleraNamed("ns", "galera", 3, nil)
	r.setBootstrapInProgress(g, "galera-0", "old-cid")

	pods := []corev1.Pod{makePod("galera-0", "new-cid")}
	if r.isBootstrapInProgress(g, pods) {
		t.Error("expected false when bootstrap pod has a new CID (restarted)")
	}
	// State should have been cleared
	if r.isBootstrapInProgress(g, pods) {
		t.Error("expected state to be cleared after CID mismatch")
	}
}

func TestIsBootstrapInProgress_PodGone(t *testing.T) {
	r := &GaleraReconciler{}
	g := makeGaleraNamed("ns", "galera", 3, nil)
	r.setBootstrapInProgress(g, "galera-0", "cid-0")

	// Pod list does not contain galera-0
	pods := []corev1.Pod{makePod("galera-1", "cid-1")}
	if r.isBootstrapInProgress(g, pods) {
		t.Error("expected false when bootstrap pod is no longer in pod list")
	}
	// State should have been cleared
	if r.isBootstrapInProgress(g, pods) {
		t.Error("expected state to be cleared after pod disappeared")
	}
}

func TestClearBootstrapState(t *testing.T) {
	r := &GaleraReconciler{}
	g := makeGaleraNamed("ns", "galera", 3, nil)
	r.setBootstrapInProgress(g, "galera-0", "cid-0")

	pods := []corev1.Pod{makePod("galera-0", "cid-0")}
	if !r.isBootstrapInProgress(g, pods) {
		t.Fatal("precondition: bootstrap should be in progress")
	}

	r.clearBootstrapState(g)
	if r.isBootstrapInProgress(g, pods) {
		t.Error("expected false after clearBootstrapState")
	}
}

func TestBootstrapState_MultipleInstances(t *testing.T) {
	r := &GaleraReconciler{}
	g1 := makeGaleraNamed("ns", "cell1-galera", 3, nil)
	g2 := makeGaleraNamed("ns", "cell2-galera", 3, nil)

	r.setBootstrapInProgress(g1, "cell1-galera-0", "cid-1")

	pods1 := []corev1.Pod{makePod("cell1-galera-0", "cid-1")}
	pods2 := []corev1.Pod{makePod("cell2-galera-0", "cid-2")}

	if !r.isBootstrapInProgress(g1, pods1) {
		t.Error("expected true for cell1")
	}
	if r.isBootstrapInProgress(g2, pods2) {
		t.Error("expected false for cell2 (no bootstrap set)")
	}

	// Setting bootstrap for cell2 should not affect cell1
	r.setBootstrapInProgress(g2, "cell2-galera-0", "cid-2")
	if !r.isBootstrapInProgress(g1, pods1) {
		t.Error("cell1 bootstrap should still be in progress")
	}
	if !r.isBootstrapInProgress(g2, pods2) {
		t.Error("cell2 bootstrap should now be in progress")
	}

	// Clearing cell1 should not affect cell2
	r.clearBootstrapState(g1)
	if r.isBootstrapInProgress(g1, pods1) {
		t.Error("cell1 should be cleared")
	}
	if !r.isBootstrapInProgress(g2, pods2) {
		t.Error("cell2 should still be in progress")
	}
}

// TestMCPRollout_MajorityBootstrapWithInitPod reproduces the MCP rollout
// scenario where one pod is stuck in Init on a rebooting node, and verifies
// that the majority-quorum fallback allows bootstrap from the available pods.
func TestMCPRollout_MajorityBootstrapWithInitPod(t *testing.T) {
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "29919", ContainerID: "cid-0", UUID: "3eec1139-7f2e-11f1-8576-1aec2611b4b0"},
		"galera-2": {Seqno: "29919", ContainerID: "cid-2", UUID: "3eec1139-7f2e-11f1-8576-1aec2611b4b0"},
	})

	pods := []corev1.Pod{
		makePodWithPhase("galera-0", "cid-0", corev1.PodRunning),
		makePodWithPhase("galera-1", "", corev1.PodPending), // Init:0/1
		makePodWithPhase("galera-2", "cid-2", corev1.PodRunning),
	}

	node, found := findBestCandidate(g, pods, ctrl.Log)
	if !found {
		t.Fatal("majority-quorum should allow bootstrap when unreported pod is Pending")
	}
	if node != "galera-0" && node != "galera-2" {
		t.Errorf("expected galera-0 or galera-2 (same seqno), got %s", node)
	}
}

// TestMCPRollout_SafeToBootstrapBypassesReplicaCheck verifies that when
// one of the available pods has SafeToBootstrap=true, findBestCandidate
// returns it immediately without waiting for all replicas.
// This is the ONE code path that could save the MCP rollout scenario.
func TestMCPRollout_SafeToBootstrapBypassesReplicaCheck(t *testing.T) {
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "29919", ContainerID: "cid-0", SafeToBootstrap: true},
		"galera-2": {Seqno: "29919", ContainerID: "cid-2"},
		// galera-1 has NO attributes
	})

	pods := []corev1.Pod{
		makePodWithPhase("galera-0", "cid-0", corev1.PodRunning),
		makePodWithPhase("galera-1", "", corev1.PodPending),
		makePodWithPhase("galera-2", "cid-2", corev1.PodRunning),
	}

	node, found := findBestCandidate(g, pods, ctrl.Log)
	if !found {
		t.Fatal("SafeToBootstrap should allow bootstrap even with missing replicas")
	}
	if node != "galera-0" {
		t.Errorf("expected galera-0 (SafeToBootstrap=true), got %s", node)
	}
}

// TestMCPRollout_IsPodRunningSkipsInitPods verifies that isPodRunning
// returns false for pods stuck in Init phase, which means the operator
// won't try to probe them or inject gcomm URIs.
func TestMCPRollout_IsPodRunningSkipsInitPods(t *testing.T) {
	initPod := makePodWithPhase("galera-1", "", corev1.PodPending)
	if isPodRunning(&initPod) {
		t.Error("isPodRunning should return false for Init/Pending pods")
	}

	runningPod := makePodWithPhase("galera-0", "cid-0", corev1.PodRunning)
	if !isPodRunning(&runningPod) {
		t.Error("isPodRunning should return true for Running pods")
	}
}

// TestMCPRollout_GetReadyPodsSkipsNonRunning verifies that getReadyPods
// filters out pods in Init/Pending phase, which matters for determining
// the Galera cluster state during rolling reboots.
func TestMCPRollout_GetReadyPodsSkipsNonRunning(t *testing.T) {
	pods := []corev1.Pod{
		makePodWithPhase("galera-0", "cid-0", corev1.PodRunning),
		makePodWithPhase("galera-1", "", corev1.PodPending),
		makePodWithPhase("galera-2", "cid-2", corev1.PodRunning),
	}
	// None of the pods have a Ready condition set, so getReadyPods
	// returns none even for Running pods. This is correct — Running
	// but not Ready means galera hasn't started.
	ready := getReadyPods(pods)
	if len(ready) != 0 {
		t.Errorf("expected 0 ready pods (none have Ready condition), got %d", len(ready))
	}
}

// Tests for majority-quorum bootstrap (Fix 1)

func TestFindBestCandidate_MajorityBootstrap_DifferentUUID(t *testing.T) {
	// Split-brain guard: 2/3 reported but with different UUIDs.
	// Must NOT allow bootstrap — different UUIDs indicate a partition.
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "100", ContainerID: "cid-0", UUID: "uuid-A"},
		"galera-2": {Seqno: "100", ContainerID: "cid-2", UUID: "uuid-B"},
	})
	pods := []corev1.Pod{
		makePodWithPhase("galera-0", "cid-0", corev1.PodRunning),
		makePodWithPhase("galera-1", "", corev1.PodPending),
		makePodWithPhase("galera-2", "cid-2", corev1.PodRunning),
	}
	_, found := findBestCandidate(g, pods, ctrl.Log)
	if found {
		t.Error("should NOT allow majority bootstrap when UUIDs differ (split-brain)")
	}
}

func TestFindBestCandidate_MajorityBootstrap_UnreportedRunningPod(t *testing.T) {
	// 2/3 reported, but the 3rd pod is Running (not Pending/Init).
	// It might still push state, so we must wait.
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "100", ContainerID: "cid-0", UUID: "uuid-A"},
		"galera-2": {Seqno: "100", ContainerID: "cid-2", UUID: "uuid-A"},
	})
	pods := []corev1.Pod{
		makePod("galera-0", "cid-0"),   // Running
		makePod("galera-1", "cid-1"),   // Running but hasn't pushed
		makePod("galera-2", "cid-2"),   // Running
	}
	_, found := findBestCandidate(g, pods, ctrl.Log)
	if found {
		t.Error("should NOT allow majority bootstrap when unreported pod is Running")
	}
}

func TestFindBestCandidate_MajorityBootstrap_PicksHighestSeqno(t *testing.T) {
	// 2/3 reported with same UUID but different seqno.
	// Should pick the one with the highest seqno.
	g := makeGalera(3, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "100", ContainerID: "cid-0", UUID: "uuid-A"},
		"galera-2": {Seqno: "200", ContainerID: "cid-2", UUID: "uuid-A"},
	})
	pods := []corev1.Pod{
		makePodWithPhase("galera-0", "cid-0", corev1.PodRunning),
		makePodWithPhase("galera-1", "", corev1.PodPending),
		makePodWithPhase("galera-2", "cid-2", corev1.PodRunning),
	}
	node, found := findBestCandidate(g, pods, ctrl.Log)
	if !found {
		t.Fatal("should allow majority bootstrap with same UUID")
	}
	if node != "galera-2" {
		t.Errorf("expected galera-2 (highest seqno=200), got %s", node)
	}
}

func TestFindBestCandidate_MajorityBootstrap_SingleReplica(t *testing.T) {
	// Single replica cluster, pod is Pending. Majority = 1, known = 0.
	g := makeGalera(1, map[string]mariadbv1.GaleraAttributes{})
	pods := []corev1.Pod{
		makePodWithPhase("galera-0", "", corev1.PodPending),
	}
	_, found := findBestCandidate(g, pods, ctrl.Log)
	if found {
		t.Error("should NOT bootstrap single replica when no pods reported")
	}
}

func TestFindBestCandidate_MajorityBootstrap_FiveNodes(t *testing.T) {
	// 5-node cluster, 3/5 reported (majority), 2 Pending.
	g := makeGalera(5, map[string]mariadbv1.GaleraAttributes{
		"galera-0": {Seqno: "500", ContainerID: "cid-0", UUID: "uuid-A"},
		"galera-2": {Seqno: "500", ContainerID: "cid-2", UUID: "uuid-A"},
		"galera-4": {Seqno: "500", ContainerID: "cid-4", UUID: "uuid-A"},
	})
	pods := []corev1.Pod{
		makePodWithPhase("galera-0", "cid-0", corev1.PodRunning),
		makePodWithPhase("galera-1", "", corev1.PodPending),
		makePodWithPhase("galera-2", "cid-2", corev1.PodRunning),
		makePodWithPhase("galera-3", "", corev1.PodPending),
		makePodWithPhase("galera-4", "cid-4", corev1.PodRunning),
	}
	node, found := findBestCandidate(g, pods, ctrl.Log)
	if !found {
		t.Fatal("should allow majority bootstrap with 3/5 reporting, same UUID")
	}
	t.Logf("Selected node: %s", node)
}

// Tests for A/P failover (Fix 2)

func makePodReady(name, containerID string) corev1.Pod {
	pod := makePod(name, containerID)
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	return pod
}

func TestSelectActivePod_CurrentIsReady(t *testing.T) {
	readyPods := []corev1.Pod{
		makePodReady("galera-0", "cid-0"),
		makePodReady("galera-1", "cid-1"),
	}
	result := selectActivePod("galera-0", readyPods)
	if result != "" {
		t.Errorf("expected no change (current active is Ready), got %s", result)
	}
}

func TestSelectActivePod_CurrentNotReady(t *testing.T) {
	readyPods := []corev1.Pod{
		makePodReady("galera-1", "cid-1"),
		makePodReady("galera-2", "cid-2"),
	}
	result := selectActivePod("galera-0", readyPods)
	if result != "galera-1" {
		t.Errorf("expected galera-1 (first Ready pod), got %q", result)
	}
}

func TestSelectActivePod_NoReadyPods(t *testing.T) {
	result := selectActivePod("galera-0", nil)
	if result != "" {
		t.Errorf("expected empty (no Ready pods), got %s", result)
	}
}

func TestSelectActivePod_ActiveNotInCluster(t *testing.T) {
	readyPods := []corev1.Pod{
		makePodReady("galera-1", "cid-1"),
	}
	result := selectActivePod("galera-nonexistent", readyPods)
	if result != "galera-1" {
		t.Errorf("expected galera-1, got %q", result)
	}
}
