#!/usr/bin/env bash
#
# Pod-Level Checkpoint/Restore Example
#
# Walks through a complete checkpoint/restore cycle:
#   1. Create a pod that counts 0, 1, 2, ...
#   2. Checkpoint the pod with CRIU
#   3. Delete the pod
#   4. Restore it from the checkpoint
#   5. Verify the counter picks up where it left off (not from 0)
#
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "--- Cleaning up previous runs ---"
kubectl delete podrestore    counter-restore    --ignore-not-found 2>/dev/null
kubectl delete podcheckpoint counter-checkpoint --ignore-not-found 2>/dev/null
kubectl delete pod           counter-app        --ignore-not-found --force --grace-period=0 2>/dev/null
sleep 3

echo ""
echo "--- Step 1: Create a pod with an incrementing counter ---"
kubectl apply -f "$DIR/counter-pod.yaml"
kubectl wait --for=condition=Ready pod/counter-app --timeout=60s
sleep 5
kubectl logs counter-app --tail=5
echo ""
read -rp "Press Enter to continue..."

echo ""
echo "--- Step 2: Let the counter run ---"
sleep 10
kubectl logs counter-app --tail=5
LAST_COUNT=$(kubectl logs counter-app --tail=1 | grep -oP '\d+')
echo "Counter is at: $LAST_COUNT"
echo ""
read -rp "Press Enter to checkpoint the pod..."

echo ""
echo "--- Step 3: Checkpoint the pod ---"
kubectl apply -f "$DIR/checkpoint.yaml"

# Wait for the checkpoint to finish.
for _ in $(seq 1 30); do
    phase=$(kubectl get podcheckpoint counter-checkpoint -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [ "$phase" = "Ready" ] && break
    [ "$phase" = "Failed" ] && { echo "Checkpoint failed"; exit 1; }
    sleep 1
done

kubectl get podcheckpoint counter-checkpoint
echo ""
echo "The pod kept running (checkpointing is non-disruptive):"
kubectl logs counter-app --tail=3
echo ""
read -rp "Press Enter to delete the pod..."

echo ""
echo "--- Step 4: Delete the pod ---"
kubectl delete pod counter-app --force --grace-period=0
sleep 5
echo "Pod deleted."
echo ""
read -rp "Press Enter to restore from checkpoint..."

echo ""
echo "--- Step 5: Restore the pod from checkpoint ---"
kubectl apply -f "$DIR/restore.yaml"

# Wait for the restore to finish.
for _ in $(seq 1 60); do
    phase=$(kubectl get podrestore counter-restore -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [ "$phase" = "Completed" ] && break
    [ "$phase" = "Failed" ] && { echo "Restore failed"; exit 1; }
    sleep 1
done

sleep 5
kubectl get pod counter-app
echo ""

echo "--- Step 6: Verify the counter continued ---"
echo ""
echo "Counter output after restore:"
kubectl logs counter-app --tail=10
echo ""

# The log includes output from before and after restore. The duplicate
# count value marks the exact point where CRIU resumed the process.
resumed_at=$(kubectl logs counter-app | grep -oP '\d+' | uniq -d | head -1)
[ -z "$resumed_at" ] && resumed_at=$(kubectl logs counter-app --tail=1 | grep -oP '\d+')

echo "Counter was at $LAST_COUNT before checkpoint, resumed at $resumed_at."
echo ""

echo "--- Step 7: Confirm stability (15 seconds) ---"
sleep 15
kubectl get pod counter-app
echo ""
kubectl logs counter-app --tail=3
