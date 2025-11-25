# Galera Operator Design and Bootstrap Logic

This document provides a comprehensive explanation of how the MariaDB Galera operator works, with a focus on the bootstrap logic and state management.

## Table of Contents

1. [Overview](#overview)
2. [Galera Cluster Basics](#galera-cluster-basics)
3. [Operator Architecture](#operator-architecture)
4. [Bootstrap State Machine](#bootstrap-state-machine)
5. [Pod Attribute Management](#pod-attribute-management)
6. [Bootstrap Selection Algorithm](#bootstrap-selection-algorithm)
7. [Failure Handling and Recovery](#failure-handling-and-recovery)
8. [Code Walkthrough](#code-walkthrough)
9. [Common Scenarios](#common-scenarios)

---

## Overview

The MariaDB Galera operator manages MariaDB Galera clusters on Kubernetes. Its primary responsibilities include:

- **Bootstrap orchestration**: Selecting the correct node to bootstrap a new cluster
- **Join coordination**: Coordinating nodes joining an existing cluster
- **Failure recovery**: Handling pod restarts and failures without data loss
- **State synchronization**: Tracking cluster state across pod lifecycles

### Key Challenge

Galera requires special handling during cluster startup. Unlike traditional primary-replica databases, Galera uses multi-master replication where **any node can be the bootstrap node**, but we must choose **the node with the most recent data** (highest sequence number) to prevent data loss.

---

## Galera Cluster Basics

### Sequence Number (Seqno)

Each Galera node tracks a **sequence number** (`seqno`) representing the last committed transaction. This is persisted in `/var/lib/mysql/grastate.dat`:

```yaml
# GALERA saved state
version: 2.1
uuid:    d38e8a2c-1234-5678-abcd-ef1234567890
seqno:   200
safe_to_bootstrap: 0
```

**Critical:** The node with the **highest seqno** has the most recent data and should be the bootstrap node.

### Bootstrap Process

1. **Cold Start**: No cluster exists, need to select a bootstrap node
   - Node with highest seqno bootstraps with `gcomm://` (empty)
   - Other nodes join with `gcomm://bootstrap-node,other-nodes,...`

2. **Normal Join**: Cluster exists, node joins existing cluster
   - Node connects with full `gcomm://` URI listing all cluster members
   - Galera performs State Snapshot Transfer (SST) or Incremental State Transfer (IST)

### Readiness States

A Galera node can be in various states:
- **Synced**: Fully synchronized, can serve queries
- **Syncing**: Performing IST, receiving incremental updates (still operational)
- **Joiner**: Performing SST, receiving full snapshot (cannot serve queries)
- **Donor**: Providing data to another node (can/cannot serve depending on method)

---

## Operator Architecture

### Controller Structure

```go
type GaleraReconciler struct {
    client.Client
    Kclient kubernetes.Interface
    config  *rest.Config
    Scheme  *runtime.Scheme
}
```

The operator follows the Kubernetes controller pattern, continuously reconciling desired state with actual state.

### Status Tracking

The operator maintains state in the `Galera` custom resource:

```go
type GaleraStatus struct {
    // Whether the cluster has been bootstrapped
    Bootstrapped bool `json:"bootstrapped"`

    // Tracked attributes for each pod
    Attributes map[string]GaleraAttributes `json:"attributes,omitempty"`

    // Pod marked as safe to bootstrap
    SafeToBootstrap string `json:"safeToBootstrap,omitempty"`

    // Standard conditions
    Conditions condition.Conditions `json:"conditions,omitempty"`
}

type GaleraAttributes struct {
    UUID            string `json:"uuid,omitempty"`
    Seqno           string `json:"seqno"`              // Persistent data
    SafeToBootstrap bool   `json:"safe_to_bootstrap,omitempty"` // Persistent data
    NoGrastate      bool   `json:"no_grastate,omitempty"`
    Gcomm           string `json:"gcomm,omitempty"`    // Runtime state
    ContainerID     string `json:"containerID,omitempty"` // Runtime state
}
```

**Key Distinction:**
- **Persistent data** (Seqno, UUID, SafeToBootstrap): Survives pod restarts, stored on persistent volume
- **Runtime state** (Gcomm, ContainerID): Changes on pod restart, represents current execution state

---

## Bootstrap State Machine

### State Diagram

```
┌─────────────┐
│   START     │
│ (Cluster    │
│  Created)   │
└──────┬──────┘
       │
       ▼
┌─────────────────────────────┐
│  PROBING PHASE              │
│  - Wait for pods Running    │
│  - Retrieve seqno from each │
│  - Store in Attributes      │
└──────┬──────────────────────┘
       │
       │ All pods probed?
       ▼
┌─────────────────────────────┐
│  CANDIDATE SELECTION        │
│  - Check SafeToBootstrap    │
│  - Find highest seqno       │
│  - Select bootstrap node    │
└──────┬──────────────────────┘
       │
       │ Candidate found?
       ▼
┌─────────────────────────────┐
│  BOOTSTRAP INJECTION        │
│  - Inject gcomm:// to node  │
│  - Mark as bootstrapping    │
│  - Wait for Ready           │
└──────┬──────────────────────┘
       │
       │ Bootstrap Ready?
       ▼
┌─────────────────────────────┐
│  JOINER INJECTION           │
│  - Other pods join cluster  │
│  - Full gcomm:// URI        │
│  - Wait for all Ready       │
└──────┬──────────────────────┘
       │
       │ All pods Ready?
       ▼
┌─────────────────────────────┐
│  RUNNING                    │
│  - Cluster operational      │
│  - Monitor health           │
└─────────────────────────────┘
```

### State Transitions

The operator determines the current state by examining:
1. `instance.Status.Bootstrapped` - Has at least one pod become Ready?
2. `isBootstrapInProgress(instance)` - Is any pod currently bootstrapping?
3. Pod readiness states
4. Attribute completeness

---

## Pod Attribute Management

### Probing Mechanism

When a pod is in `Running` phase but not `Ready`, the operator probes it:

```go
func getRunningPodsMissingAttributes(ctx context.Context, pods []corev1.Pod,
    instance *mariadbv1.Galera, h *helper.Helper, config *rest.Config) (ret []corev1.Pod) {

    for _, pod := range pods {
        if pod.Status.Phase == corev1.PodRunning && !podutils.IsPodReady(&pod) {
            _, attrFound := instance.Status.Attributes[pod.Name]
            if !attrFound && isGaleraContainerStartedAndWaiting(ctx, &pod, instance, h, config) {
                ret = append(ret, pod)
            }
        }
    }
    return
}
```

The probe checks if the pod's init script is waiting for the `gcomm_uri` file:

```go
func isGaleraContainerStartedAndWaiting(ctx context.Context, pod *corev1.Pod,
    instance *mariadbv1.Galera, h *helper.Helper, config *rest.Config) bool {

    waiting := false
    err := mariadb.ExecInPod(ctx, h, config, instance.Namespace, pod.Name, "galera",
        []string{"/bin/bash", "-c",
            "test ! -f /var/lib/mysql/gcomm_uri && pgrep -aP1 | grep -o detect_gcomm_and_start.sh"},
        func(stdout *bytes.Buffer, _ *bytes.Buffer) error {
            predicate := strings.TrimSuffix(stdout.String(), "\n")
            waiting = (predicate == "detect_gcomm_and_start.sh")
            return nil
        })
    return err == nil && waiting
}
```

### Sequence Number Retrieval

Once a pod is ready for probing, the operator executes a script to retrieve its seqno:

```go
func retrieveSequenceNumber(ctx context.Context, helper *helper.Helper, config *rest.Config,
    instance *mariadbv1.Galera, pod *corev1.Pod) (errStr []string, err error) {

    err = mariadb.ExecInPod(ctx, helper, config, instance.Namespace, pod.Name, "galera",
        []string{"/bin/bash", "/var/lib/operator-scripts/detect_last_commit.sh"},
        func(stdout *bytes.Buffer, stderr *bytes.Buffer) error {
            var attr mariadbv1.GaleraAttributes
            if err := json.Unmarshal(stdout.Bytes(), &attr); err != nil {
                return err
            }
            instance.Status.Attributes[pod.Name] = attr
            return nil
        })
    return
}
```

The script reads `/var/lib/mysql/grastate.dat` and returns JSON:

```json
{
  "uuid": "d38e8a2c-1234-5678-abcd-ef1234567890",
  "seqno": "200",
  "safe_to_bootstrap": false,
  "no_grastate": false
}
```

### Attribute Lifecycle

**Normal Flow:**
1. Pod starts → `Running` but not `Ready`
2. Operator probes → retrieves seqno
3. Operator stores in `Status.Attributes[pod.Name]`
4. Bootstrap/join decision made
5. Operator injects `gcomm_uri`
6. Pod starts galera → becomes `Ready`
7. Operator clears attributes (no longer needed)

**Failure Flow (OLD - BUGGY):**
1. Pod starts → probed → seqno retrieved
2. Bootstrap injection attempted → fails
3. Operator calls `clearPodAttributes()` → **ALL DATA LOST**
4. Next reconcile → pod has no seqno → wrong bootstrap decision → **DATA LOSS**

**Failure Flow (NEW - FIXED):**
1. Pod starts → probed → seqno retrieved
2. Bootstrap injection attempted → fails
3. Operator calls `clearPodRuntimeState()` → **SEQNO PRESERVED**
4. Next reconcile → pod still has seqno → correct bootstrap decision → **NO DATA LOSS**

---

## Bootstrap Selection Algorithm

### The `findBestCandidate` Function

This is the critical function that selects which pod should bootstrap the cluster:

```go
func findBestCandidate(g *mariadbv1.Galera) (node string, found bool) {
    sortednodes := maps.Keys(g.Status.Attributes)
    sort.Strings(sortednodes)
    bestnode := ""
    bestseqno := -1

    for _, node := range sortednodes {
        // Priority 1: SafeToBootstrap flag set by Galera on clean shutdown
        if g.Status.Attributes[node].SafeToBootstrap {
            return node, true
        }

        // Priority 2: Highest sequence number
        seqno := g.Status.Attributes[node].Seqno
        intseqno, _ := strconv.Atoi(seqno)
        if intseqno >= bestseqno {
            bestnode = node
            bestseqno = intseqno
        }
    }

    // SAFETY CHECK: Only proceed if we have ALL replicas probed
    // This prevents data loss from incomplete information
    if len(g.Status.Attributes) != int(*g.Spec.Replicas) {
        return "", false
    }

    return bestnode, true
}
```

### Selection Priority

1. **SafeToBootstrap flag**: If Galera set this flag during clean shutdown, use that node
   - Galera sets this on the last node to leave the cluster
   - Most reliable indicator when available

2. **Highest seqno**: Node with most recent data
   - Critical for preventing data loss
   - Used when SafeToBootstrap is not available (unclean shutdown)

3. **All replicas probed**: Safety check
   - Waits for complete information
   - Prevents bootstrapping with partial data that could lead to wrong choice

### Example Scenarios

**Scenario 1: Clean Shutdown**
```
galera-0: seqno=100, safe_to_bootstrap=false
galera-1: seqno=150, safe_to_bootstrap=false
galera-2: seqno=150, safe_to_bootstrap=true  ← Selected (SafeToBootstrap priority)
```

**Scenario 2: Unclean Shutdown**
```
galera-0: seqno=100, safe_to_bootstrap=false
galera-1: seqno=150, safe_to_bootstrap=false  ← Selected (highest seqno)
galera-2: seqno=120, safe_to_bootstrap=false
```

**Scenario 3: Incomplete Data (WAIT)**
```
galera-0: seqno=100
galera-1: seqno=150
galera-2: [NOT PROBED YET]

Result: found=false (wait for galera-2 to be probed)
```

---

## Failure Handling and Recovery

### The Critical Problem

When a pod fails during bootstrap and restarts, the operator must:
1. **Detect the failure** - Notice the container ID changed
2. **Clear runtime state** - Allow retry of bootstrap
3. **Preserve persistent data** - Remember which pod has the most recent data

### The `clearPodRuntimeState` Solution

```go
func clearPodRuntimeState(instance *mariadbv1.Galera, podName string) {
    attr, found := instance.Status.Attributes[podName]
    if !found {
        return
    }
    // Clear only ephemeral runtime state
    attr.Gcomm = ""
    attr.ContainerID = ""
    // Preserve persistent data: UUID, Seqno, SafeToBootstrap, NoGrastate
    instance.Status.Attributes[podName] = attr
}
```

**Why This Works:**
- `Seqno` is read from persistent volume (survives restart)
- `ContainerID` changes on restart (detects failure)
- Clearing `Gcomm` allows `isBootstrapInProgress()` to return false
- Preserving `Seqno` ensures correct bootstrap selection

### Failure Detection

```go
func assertPodsAttributesValidity(helper *helper.Helper, instance *mariadbv1.Galera, pods []corev1.Pod) {
    for _, pod := range pods {
        _, found := instance.Status.Attributes[pod.Name]
        if !found {
            continue
        }

        // Compare stored ContainerID with current pod's ContainerID
        attrCID := instance.Status.Attributes[pod.Name].ContainerID
        containerFound, podCID := getGaleraContainerID(&pod)

        if !containerFound || (attrCID != "" && attrCID != podCID) {
            // Pod restarted! Clear runtime state but preserve seqno
            clearPodRuntimeState(instance, pod.Name)
            util.LogForObject(helper, "Pod restarted while galera was starting, clearing runtime state",
                instance, "pod", pod.Name, "recorded ID", attrCID)
        }
    }
}
```

### Recovery Flow

**Timeline of Events:**

```
T0: Initial cluster start
    galera-0: seqno=100
    galera-1: seqno=150
    galera-2: seqno=200  ← Selected for bootstrap

T1: Bootstrap injection
    galera-2: Gcomm="gcomm://", ContainerID="abc123"

T2: Bootstrap fails
    galera-2: Pod crashes, restarts

T3: Operator detects failure
    assertPodsAttributesValidity() sees ContainerID changed
    Calls clearPodRuntimeState(galera-2)
    galera-2: Gcomm="", ContainerID="", seqno=200 (PRESERVED!)

T4: Next reconcile
    findBestCandidate() still sees galera-2 with seqno=200
    Selects galera-2 again (correct choice!)

T5: Retry injection
    galera-2: Now Running and ready
    Injection succeeds
    Bootstrap completes
```

### Why the Old Code Failed

**Old Code (WRONG):**
```go
if !containerFound || (attrCID != "" && attrCID != podCID) {
    clearPodAttributes(instance, pod.Name)  // ← Deletes ALL data!
    ...
}

func clearPodAttributes(instance *mariadbv1.Galera, podName string) {
    delete(instance.Status.Attributes, podName)  // ← Seqno lost!
}
```

**Data Loss Scenario:**
```
T3: Operator detects failure
    Calls clearPodAttributes(galera-2)
    galera-2: NO ATTRIBUTES AT ALL

T4: Next reconcile
    Only galera-0 (seqno=100) and galera-1 (seqno=150) in attributes
    If galera-2 is slow to restart, not in Running phase yet
    findBestCandidate() selects galera-1
    WRONG CHOICE! 50 transactions lost!
```

---

## Code Walkthrough

### Main Reconcile Loop

The reconcile function orchestrates the entire bootstrap process:

```go
func (r *GaleraReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // 1. Fetch the Galera instance
    instance := &mariadbv1.Galera{}
    err := r.Get(ctx, req.NamespacedName, instance)
    if err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // 2. Ensure StatefulSet exists
    statefulset, err := r.ensureStatefulSet(ctx, instance)
    if err != nil {
        return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
    }

    // 3. Get all pods
    podList, err := r.getPods(ctx, instance)
    if err != nil {
        return ctrl.Result{}, err
    }

    // 4. CRITICAL: Validate pod attributes (detect restarts)
    assertPodsAttributesValidity(helper, instance, podList.Items)

    // 5. Determine if cluster is bootstrapped
    instance.Status.Bootstrapped = statefulset.Status.AvailableReplicas > 0

    // 6. Handle bootstrapped cluster (joiner logic)
    if instance.Status.Bootstrapped {
        return r.handleBootstrappedCluster(ctx, instance, podList)
    }

    // 7. Handle bootstrap phase
    return r.handleBootstrapPhase(ctx, instance, podList)
}
```

### Bootstrap Phase Handler

```go
func (r *GaleraReconciler) handleBootstrapPhase(ctx context.Context,
    instance *mariadbv1.Galera, podList *corev1.PodList) (ctrl.Result, error) {

    // Skip if bootstrap already in progress
    if isBootstrapInProgress(instance) {
        return r.waitForBootstrap()
    }

    // Probe any pods that don't have attributes yet
    for _, pod := range getRunningPodsMissingAttributes(ctx, podList.Items, instance, h, r.config) {
        log.Info("Pod running, retrieve seqno", "pod", pod.Name)
        warn, err := retrieveSequenceNumber(ctx, h, r.config, instance, &pod)
        if err != nil {
            return ctrl.Result{}, err
        }

        // If this pod is marked SafeToBootstrap, use it immediately
        if instance.Status.Attributes[pod.Name].SafeToBootstrap {
            return r.bootstrapWithNode(ctx, instance, pod.Name)
        }
    }

    // Find best candidate from all probed pods
    node, found := findBestCandidate(instance)
    if !found {
        // Not all pods probed yet, wait
        return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
    }

    // Inject bootstrap URI
    return r.bootstrapWithNode(ctx, instance, node)
}
```

### Bootstrap Injection

```go
func (r *GaleraReconciler) bootstrapWithNode(ctx context.Context,
    instance *mariadbv1.Galera, nodeName string) (ctrl.Result, error) {

    pod := getPodFromName(podList.Items, nodeName)
    if pod == nil {
        log.Info("Bootstrap candidate pod not found, will retry", "pod", nodeName)
        return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
    }

    log.Info("Pushing gcomm URI to bootstrap", "pod", nodeName)
    err := injectGcommURI(ctx, h, r.config, instance, pod, "gcomm://")
    if err != nil {
        log.Error(err, "Failed to push gcomm URI", "pod", nodeName)
        // CRITICAL: Preserve seqno for retry
        clearPodRuntimeState(instance, nodeName)
        return ctrl.Result{}, err
    }

    return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
}
```

### Gcomm URI Injection

The operator writes the `gcomm_uri` file that the pod's init script is waiting for:

```go
func injectGcommURI(ctx context.Context, h *helper.Helper, config *rest.Config,
    instance *mariadbv1.Galera, pod *corev1.Pod, uri string) error {

    err := mariadb.ExecInPod(ctx, h, config, instance.Namespace, pod.Name, "galera",
        []string{"/bin/bash", "-c", "echo '" + uri + "' > /var/lib/mysql/gcomm_uri"},
        func(_ *bytes.Buffer, _ *bytes.Buffer) error {
            // Update status to track this injection
            attr := instance.Status.Attributes[pod.Name]
            attr.Gcomm = uri
            attr.ContainerID = pod.Status.ContainerStatuses[0].ContainerID
            instance.Status.Attributes[pod.Name] = attr
            return nil
        })
    return err
}
```

**What happens in the pod:**
1. Init script `detect_gcomm_and_start.sh` is waiting in a loop
2. Operator writes `/var/lib/mysql/gcomm_uri`
3. Init script detects file, reads URI
4. Starts MariaDB with `wsrep_cluster_address=<URI>`
5. Galera begins bootstrap process

---

## Common Scenarios

### Scenario 1: Fresh Cluster Start

**Initial State:**
- 3 pods created by StatefulSet
- All in `Pending` → `Running` phase
- None are `Ready`

**Operator Flow:**

1. **Reconcile T1**: Pods not Running yet
   - No action taken
   - Requeue for 3 seconds

2. **Reconcile T2**: All pods Running
   - `getRunningPodsMissingAttributes` returns all 3 pods
   - Probes galera-0: seqno=0
   - Probes galera-1: seqno=0
   - Probes galera-2: seqno=0

3. **Reconcile T3**: All probed
   - `findBestCandidate` returns galera-2 (deterministic via sort)
   - Injects `gcomm://` to galera-2
   - Status: `Bootstrapped=false`, `isBootstrapInProgress=true`

4. **Reconcile T4**: Bootstrap in progress
   - Waits for galera-2 to become Ready
   - Requeue for 3 seconds

5. **Reconcile T5**: galera-2 Ready!
   - `Bootstrapped=true` (AvailableReplicas > 0)
   - Other pods (galera-0, galera-1) need to join
   - Injects `gcomm://galera-2,galera-1,galera-0` to each

6. **Reconcile T6**: All Ready
   - Cluster operational
   - Clears all attributes (no longer needed)

### Scenario 2: Bootstrap Failure with Recovery

**Initial State:**
- 3 pods, seqno: galera-0=100, galera-1=150, galera-2=200

**Operator Flow:**

1. **T1**: All probed, galera-2 selected for bootstrap
   - Injects `gcomm://` to galera-2
   - Status: `Attributes[galera-2].Gcomm="gcomm://"`, `ContainerID="abc123"`

2. **T2**: galera-2 crashes during bootstrap
   - Pod restarts, new ContainerID="def456"
   - Galera failed to start

3. **T3**: Operator detects restart
   - `assertPodsAttributesValidity` sees ContainerID mismatch
   - Calls `clearPodRuntimeState(galera-2)`
   - Status: `Attributes[galera-2].Gcomm=""`, `ContainerID=""`, `Seqno=200` ✓

4. **T4**: Retry bootstrap
   - `isBootstrapInProgress` returns false (Gcomm cleared)
   - `findBestCandidate` still returns galera-2 (seqno preserved!)
   - Injects `gcomm://` again when pod is Running

5. **T5**: Bootstrap succeeds
   - galera-2 becomes Ready
   - Other pods join
   - Cluster operational

**Key Point**: Without `clearPodRuntimeState`, we would have lost the seqno and potentially selected the wrong node!

### Scenario 3: One Pod Slow to Start

**Initial State:**
- galera-0: Running, seqno=100
- galera-1: Running, seqno=150
- galera-2: Pending (image pull, etc.)

**Operator Flow:**

1. **T1**: Probe running pods
   - Retrieves seqno from galera-0 and galera-1
   - galera-2 not Running yet, can't probe

2. **T2**: Check for bootstrap candidate
   - `findBestCandidate` called
   - Only 2/3 attributes present
   - **Returns `found=false`** (safety check)
   - Waits for all pods

3. **T3**: galera-2 becomes Running
   - Probes galera-2, gets seqno=200
   - Now 3/3 attributes present

4. **T4**: Select bootstrap candidate
   - `findBestCandidate` returns galera-2
   - Bootstrap proceeds correctly!

**Key Point**: The safety check `len(g.Status.Attributes) != int(*g.Spec.Replicas)` prevents premature bootstrap with incomplete data.

### Scenario 4: Pod Deleted and Recreated

**Initial State:**
- Cluster running, 3/3 Ready
- User deletes galera-1 pod
- StatefulSet recreates it

**Operator Flow:**

1. **T1**: galera-1 deleted
   - Cluster still operational (2/3 nodes)
   - StatefulSet creates new pod

2. **T2**: New galera-1 Running but not Ready
   - Cluster is `Bootstrapped=true`
   - Operator treats it as joiner

3. **T3**: Inject joiner URI
   - Injects `gcomm://galera-0,galera-1,galera-2`
   - galera-1 performs SST/IST from cluster
   - Becomes Ready and joins cluster

**Key Point**: Once `Bootstrapped=true`, all pods use joiner logic, not bootstrap logic.

---

## Readiness Probe Fix

### The Problem

The original readiness probe was too strict:

```bash
# OLD CODE (WRONG)
check_mysql_status wsrep_local_state_comment Synced
```

This only accepted the `Synced` state, causing issues during IST (Incremental State Transfer):

1. Node starts IST → state becomes `Syncing`
2. Readiness probe fails
3. Pod marked NotReady
4. If multiple nodes are syncing, cluster loses quorum
5. **Cascading failure!**

### The Fix

```bash
# NEW CODE (CORRECT)
comment=$(get_mysql_status wsrep_local_state_comment)
test "${comment}" = "Synced" -o "${comment}" = "Syncing"
```

Now accepts both `Synced` and `Syncing` states:

- `Synced`: Node fully synchronized
- `Syncing`: Node performing IST (receiving incremental updates, **still operational**)

### Why Syncing is Safe

During IST, the node:
- ✅ Can serve read queries
- ✅ Can accept write queries
- ✅ Is part of the cluster quorum
- ✅ Is applying incremental changes in background

It's only during SST (full snapshot transfer) that a node is in `Joiner` state and cannot serve traffic.

---

## Summary

### Key Design Principles

1. **Data Safety First**: Always preserve seqno to ensure correct bootstrap selection
2. **Wait for Complete Information**: Don't bootstrap until all pods are probed
3. **Distinguish Runtime vs Persistent State**: Clear only what's ephemeral
4. **Graceful Failure Handling**: Failed operations should not lose critical data
5. **Deterministic Selection**: Sorted order ensures consistent choice when seqno is equal

### Critical Functions

| Function | Purpose | Key Behavior |
|----------|---------|--------------|
| `findBestCandidate` | Select bootstrap node | Waits for all replicas, prefers SafeToBootstrap, then highest seqno |
| `clearPodRuntimeState` | Handle failures | Clears Gcomm/ContainerID, preserves Seqno/UUID |
| `assertPodsAttributesValidity` | Detect restarts | Compares ContainerID to detect pod failures |
| `retrieveSequenceNumber` | Probe pods | Reads grastate.dat from pod's persistent volume |
| `injectGcommURI` | Trigger bootstrap/join | Writes gcomm_uri file that pod is waiting for |
| `isBootstrapInProgress` | Check state | Returns true if any pod has Gcomm="gcomm://" |

### Data Loss Prevention

The operator prevents data loss through:

1. **Complete probing**: Waits for all pods before bootstrap decision
2. **Seqno preservation**: Retains seqno data across pod failures
3. **Highest seqno selection**: Always chooses node with most recent data
4. **SafeToBootstrap priority**: Trusts Galera's clean shutdown indicator
5. **Retry with same node**: Failed bootstrap retries with correct node

### Requeue Strategy

The operator uses strategic requeuing to poll for state changes:

```go
// Wait for pods to become available
if statefulset.Status.AvailableReplicas != statefulset.Status.Replicas {
    return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
}

// Return error for immediate retry (with exponential backoff)
if err != nil {
    return ctrl.Result{}, err
}
```

- **3-second requeue**: Normal polling for expected state changes
- **Error return**: Immediate retry with exponential backoff for unexpected failures

---

## Further Reading

- [Galera Documentation](https://galeracluster.com/library/documentation/)
- [Kubernetes Operators](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Controller Runtime](https://github.com/kubernetes-sigs/controller-runtime)

---

**Document Version**: 1.0
**Last Updated**: 2025-01-25
**Operator Version**: Includes clearPodRuntimeState fix
