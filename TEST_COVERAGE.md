# Test Coverage for Galera Fixes

This document describes the test coverage for the two critical fixes implemented in the Galera operator.

## Fixes Covered

### 1. Readiness Probe Fix (mysql_probe.sh)
**Issue**: The readiness probe was too strict, only accepting "Synced" state. This caused pods to be marked not ready during normal "Syncing" operations (Incremental State Transfer), potentially causing cascading failures.

**Fix**: Modified the readiness probe to accept both "Synced" and "Syncing" states.

**File**: `templates/galera/bin/mysql_probe.sh` (lines 206-210)

### 2. Bootstrap Retry Fix (galera_controller.go)
**Issue**: When bootstrap fails and the pod restarts, the operator was clearing all pod attributes including seqno data. This could cause two critical problems:
1. **Data Loss**: If we allow bootstrap with partial data, we might bootstrap the wrong node and lose transactions
2. **Indefinite Stall**: If we wait for all pods but keep clearing the failed pod's data, we never have complete data

**Fix**: Implemented `clearPodRuntimeState` to preserve persistent data (Seqno, UUID, SafeToBootstrap) while clearing only runtime state (Gcomm, ContainerID). This ensures:
- Bootstrap can be retried after failures
- The operator always knows which pod has the most recent data
- No data loss occurs even when pods fail during bootstrap

**Files**:
- `controllers/galera_controller.go` (lines 296-309: new `clearPodRuntimeState` function)
- `controllers/galera_controller.go` (lines 328-351: updated `assertPodsAttributesValidity`)
- `controllers/galera_controller.go` (line 878: failed joiner injection uses `clearPodRuntimeState`)
- `controllers/galera_controller.go` (lines 924-938: bootstrap injection with nil check and `clearPodRuntimeState`)

## Test Files

### Go Unit Tests
**File**: `controllers/galera_helpers_test.go`

Tests for the controller helper functions, including the critical `findBestCandidate` fix.

**Run tests:**
```bash
go test -v ./controllers -run "^Test(FindBestCandidate|BuildGcommURI|IsBootstrapInProgress|ClearPodRuntimeState)"
```

**Test cases:**

#### findBestCandidate
1. `TestFindBestCandidate_AllReplicasPresent` - Verifies normal case with all replicas
2. `TestFindBestCandidate_SafeToBootstrapTakesPriority` - Verifies SafeToBootstrap flag takes priority
3. `TestFindBestCandidate_BootstrapRetryAfterFailure` - **[CRITICAL]** Tests the fix: bootstrap retry with seqno preserved
4. `TestFindBestCandidate_SinglePodWithAttributes` - Waits for all pods to be probed before bootstrap
5. `TestFindBestCandidate_NoAttributes` - Initial state: no pods probed yet
6. `TestFindBestCandidate_SingleNodeCluster` - 1-node cluster case
7. `TestFindBestCandidate_EqualSeqno` - Deterministic selection with equal seqno

#### clearPodRuntimeState
1. `TestClearPodRuntimeState` - **[CRITICAL]** Verifies runtime state cleared while persistent data preserved
2. Handles non-existent pods gracefully

#### buildGcommURI
1. Tests correct URI generation for 1, 3, and 5-node clusters

#### isBootstrapInProgress
1. Tests detection of bootstrap in progress
2. Tests detection of nodes joining cluster
3. Tests state after bootstrap failure (runtime state cleared)

### Bash Unit Tests
**File**: `templates/galera/bin/test_readiness_probe.sh`

Tests for the readiness probe logic fix.

**Run tests:**
```bash
bash templates/galera/bin/test_readiness_probe.sh
```

**Test cases:**
1. `Accept 'Synced' state` - Normal operational state
2. `Accept 'Syncing' state (THE FIX)` - **[CRITICAL]** The fix for IST operations
3. `Reject 'Donor' state` - Node transferring data, can't serve
4. `Reject 'Joiner' state` - Node receiving data
5. `Reject 'Initializing' state` - Node starting up
6. `Reject empty state` - Invalid state
7. `Reject 'Donor/Desynced' state` - Invalid state

## Running All Tests

### Go Tests
```bash
cd /var/home/lmiccini/Code/upstream/mariadb-operator
go test -v ./controllers -run "^Test(FindBestCandidate|BuildGcommURI|IsBootstrapInProgress|ClearPodRuntimeState)"
```

Expected output: All tests should pass
```
PASS
ok      github.com/openstack-k8s-operators/mariadb-operator/controllers
```

### Bash Tests
```bash
cd /var/home/lmiccini/Code/upstream/mariadb-operator
bash templates/galera/bin/test_readiness_probe.sh
```

Expected output:
```
All tests passed!
```

## Critical Test Cases

The following test cases are critical as they directly validate the fixes:

1. **`TestFindBestCandidate_BootstrapRetryAfterFailure`**: Validates that bootstrap can be retried after failure while preserving seqno data to prevent data loss.

2. **`TestClearPodRuntimeState`**: Validates that only runtime state (Gcomm, ContainerID) is cleared while persistent data (Seqno, UUID, SafeToBootstrap) is preserved.

3. **`Accept 'Syncing' state (THE FIX)`**: Validates that the readiness probe accepts "Syncing" state to prevent cascading failures during IST.

## Test Scenarios Covered

### Bootstrap Retry Scenario

**Without the fix - Data Loss:**
1. Initial 3-node cluster starts (galera-0: seqno=100, galera-1: seqno=150, galera-2: seqno=200)
2. Controller retrieves seqno for all 3 pods
3. Controller selects galera-2 (highest seqno) and injects `gcomm://` for bootstrap
4. Bootstrap fails (e.g., pod crashes during startup)
5. `clearPodAttributes` clears ALL data for galera-2
6. If galera-2 is slow to restart, `findBestCandidate` only sees galera-0 and galera-1
7. **DATA LOSS**: Bootstrap proceeds with galera-1 (seqno=150), losing 50 transactions!

**With the fix - Data Preserved:**
1. Initial 3-node cluster starts (galera-0: seqno=100, galera-1: seqno=150, galera-2: seqno=200)
2. Controller retrieves seqno for all 3 pods
3. Controller selects galera-2 (highest seqno) and injects `gcomm://` for bootstrap
4. Bootstrap fails, galera-2 pod restarts
5. `assertPodsAttributesValidity` calls `clearPodRuntimeState` on galera-2
   - Clears: Gcomm, ContainerID (runtime state)
   - Preserves: Seqno=200, UUID, SafeToBootstrap (persistent data)
6. Even if galera-2 is slow to restart, `findBestCandidate` sees all 3 pods with seqno
7. **NO DATA LOSS**: `findBestCandidate` still selects galera-2 (highest seqno), waits for it to be ready
8. Bootstrap retries with correct node when galera-2 becomes ready

### Readiness Probe Scenario
1. Galera cluster is running normally
2. A node starts IST (Incremental State Transfer) - state becomes "Syncing"
3. **BEFORE FIX**: Readiness probe fails, pod marked NotReady, potential cascading failure
4. **AFTER FIX**: Readiness probe passes, pod stays Ready, cluster remains stable

## Additional Robustness Improvements

Beyond the core fix, additional safeguards were added:

1. **Nil Pod Check**: Added nil check when `getPodFromName` returns no pod (e.g., pod deleted or restarting)
   - Location: `controllers/galera_controller.go` (lines 924-927)
   - Prevents crashes when trying to inject gcomm into non-existent pods

2. **Failed Injection Handling**: Both bootstrap and joiner injection failures now use `clearPodRuntimeState`
   - Locations: Lines 878, 935
   - Ensures consistent behavior across all failure scenarios

## Future Improvements

Potential test enhancements:
1. Integration tests with actual Galera cluster
2. Tests for `assertPodsAttributesValidity` function
3. End-to-end tests simulating pod failures and restarts
4. Performance tests for large-scale deployments
5. Chaos engineering tests (pod deletions, network partitions, etc.)
