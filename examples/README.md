# Examples

## Pod-Level Checkpoint/Restore

Checkpoint a running pod, delete it, and restore it from the checkpoint.
The restored process continues from exactly where it was checkpointed.

### Prerequisites

- Kubernetes cluster with CRIU installed on the node
- kubelet with `KubeletLocalPodCheckpointRestore` feature gate
- containerd with CheckpointPod/RestorePod CRI support
- CRDs installed: `kubectl apply -f config/crd/bases/`
- Controller running: `make run`

### Run

```bash
./examples/checkpoint-restore.sh
```

### Manual steps

```bash
kubectl apply -f examples/counter-pod.yaml     # Create the pod
kubectl logs counter-app -f                      # Watch the counter

kubectl apply -f examples/checkpoint.yaml        # Checkpoint
kubectl get podcheckpoint counter-checkpoint -w  # Wait for Ready

kubectl delete pod counter-app --force --grace-period=0  # Delete

kubectl apply -f examples/restore.yaml           # Restore
kubectl get podrestore counter-restore -w        # Wait for Completed

kubectl logs counter-app -f                      # Counter continues
```

### Files

| File | Description |
|------|-------------|
| `counter-pod.yaml` | Pod with an incrementing counter |
| `checkpoint.yaml` | PodCheckpoint targeting the counter pod |
| `restore.yaml` | PodRestore referencing the checkpoint |
| `checkpoint-restore.sh` | Interactive walkthrough |
