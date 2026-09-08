#!/usr/bin/env bash
set -euo pipefail

RESCTRL_ROOT="${1:-/tmp/fake-resctrl}"

echo "Initializing mock resctrl filesystem at ${RESCTRL_ROOT}..."
mkdir -p "${RESCTRL_ROOT}/info/L3"
mkdir -p "${RESCTRL_ROOT}/info/MB"
mkdir -p "${RESCTRL_ROOT}/info/L3_MON"

# CAT L3
echo "16" > "${RESCTRL_ROOT}/info/L3/num_closids"
echo "fffff" > "${RESCTRL_ROOT}/info/L3/cbm_mask"
echo "1" > "${RESCTRL_ROOT}/info/L3/min_cbm_bits"

# Root schemata and tasks
printf "L3:0=fffff;1=fffff\nMB:0=100;1=100\n" > "${RESCTRL_ROOT}/schemata"
touch "${RESCTRL_ROOT}/tasks"

# MBA
echo "16" > "${RESCTRL_ROOT}/info/MB/num_closids"
echo "10" > "${RESCTRL_ROOT}/info/MB/min_bandwidth"
echo "10" > "${RESCTRL_ROOT}/info/MB/bandwidth_gran"
echo "1" > "${RESCTRL_ROOT}/info/MB/ctrl_in_percentages"

# Monitoring
printf "llc_occupancy\nmbm_local_bytes\nmbm_total_bytes\n" > "${RESCTRL_ROOT}/info/L3_MON/mon_features"

chmod -R 0777 "${RESCTRL_ROOT}"
echo "Mock resctrl filesystem created successfully."
