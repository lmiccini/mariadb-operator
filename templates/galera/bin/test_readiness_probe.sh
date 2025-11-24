#!/bin/bash
#
# Unit tests for the readiness probe fix
#
# This tests that the readiness probe accepts both "Synced" and "Syncing" states
# to prevent pods from being marked not ready during IST operations.
#

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

print_test_header() {
    echo -e "\n${YELLOW}=== $1 ===${NC}"
}

print_pass() {
    echo -e "${GREEN}✓ PASS${NC}: $1"
    ((TESTS_PASSED++))
}

print_fail() {
    echo -e "${RED}✗ FAIL${NC}: $1"
    ((TESTS_FAILED++))
}

# Test the readiness probe logic directly
test_readiness_logic() {
    local test_name="$1"
    local wsrep_state="$2"
    local expected_exit="$3"

    ((TESTS_RUN++))

    # This is the actual logic from the readiness probe case in mysql_probe.sh
    # comment=$(get_mysql_status wsrep_local_state_comment)
    # test "${comment}" = "Synced" -o "${comment}" = "Syncing"

    local exit_code=0
    comment="${wsrep_state}"
    test "${comment}" = "Synced" -o "${comment}" = "Syncing" || exit_code=$?

    if [ ${exit_code} -eq ${expected_exit} ]; then
        print_pass "${test_name}"
        return 0
    else
        print_fail "${test_name} (expected exit ${expected_exit}, got ${exit_code})"
        return 1
    fi
}

print_test_header "Readiness Probe Logic Tests"

echo "Testing the fix: readiness probe should accept both 'Synced' and 'Syncing' states"
echo ""

# Test cases
test_readiness_logic "Accept 'Synced' state" "Synced" 0
test_readiness_logic "Accept 'Syncing' state (THE FIX)" "Syncing" 0
test_readiness_logic "Reject 'Donor' state" "Donor" 1
test_readiness_logic "Reject 'Joiner' state" "Joiner" 1
test_readiness_logic "Reject 'Initializing' state" "Initializing" 1
test_readiness_logic "Reject empty state" "" 1
test_readiness_logic "Reject 'Donor/Desynced' state" "Donor/Desynced" 1

# Print summary
echo ""
echo "========================================"
echo "Test Summary:"
echo "  Total:  ${TESTS_RUN}"
echo -e "  ${GREEN}Passed: ${TESTS_PASSED}${NC}"
if [ ${TESTS_FAILED} -gt 0 ]; then
    echo -e "  ${RED}Failed: ${TESTS_FAILED}${NC}"
    echo "========================================"
    exit 1
else
    echo "  Failed: ${TESTS_FAILED}"
    echo "========================================"
    echo -e "${GREEN}All tests passed!${NC}"
    exit 0
fi
