# Pod Snapshot Controller

A Kubernetes controller that provides declarative Pod-level checkpoint and
restore. It introduces **PodCheckpoint** and **PodRestore** custom resources
that enable the checkpoint/restore lifecycle through the kubelet's APIs
([KEP-5823](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/5823-pod-level-checkpoint-restore)).

The controller is runtime-agnostic: the exact checkpoint format and low-level
implementation details are left to the container runtime (e.g., containerd,
CRI-O), the OCI runtime (runc, crun, youki), and the underlying
checkpoint/restore mechanism (e.g., [CRIU](https://criu.org),
[gVisor](https://gvisor.dev)).

This controller is being developed as part of the
[WG Checkpoint Restore](https://github.com/kubernetes/community/tree/master/wg-checkpoint-restore)
effort to enable Pod-level checkpoint/restore in Kubernetes.

## Motivation

The Pod is the fundamental unit in Kubernetes and all higher-level controllers
(Deployments, StatefulSets, Jobs) operate on Pods. Pod-level checkpoint/restore
enables use cases that are difficult to address with container-level checkpointing:

- **Warm start / cold start optimization**: Pre-initialize inference engines
  (vLLM, etc.), checkpoint after GPU state is loaded, restore in seconds
  instead of minutes. Java/JVM applications with CRaC integration. LLM
  serving: checkpoint a loaded model, spin up replicas on demand.
- **Fault tolerance for training workloads**: Periodic checkpointing of
  long-running training jobs; resume from last checkpoint on failure instead
  of restarting from scratch.
- **Resource optimization**: Idle Jupyter notebooks consuming GPUs:
  checkpoint, free resources, restore when the user returns. Inference models
  not actively serving: checkpoint and swap. Time-of-day workload patterns:
  checkpoint overnight, restore in morning.
- **Pod migration**: Move Pods between nodes for maintenance, rebalancing, or
  cost optimization (initially checkpoint + stop + restore on a different
  node; eventually live migration with minimal downtime).
- **Preemption integration**: Scheduler preempts a Pod by checkpointing
  instead of killing, allowing it to resume later.

## Design

The API is modeled after the
[Kubernetes Volume Snapshot](https://kubernetes.io/docs/concepts/storage/volume-snapshots/)
API, following guidance from SIG API Machinery. Like volume snapshots,
checkpoints are standalone objects with their own lifecycle; a checkpoint can
outlive the Pod it was created from, and a single checkpoint can be used to
create multiple Pods.

Key design decisions (from the WG Checkpoint Restore meetings):

- **Pod-level over container-level**: focus on Pod-level checkpoint/restore.
  Container-level checkpointing (currently beta) stays as-is for forensic use
  cases. VM-based runtimes (Kata, gVisor) checkpoint at Pod level, not
  container level.
- **New Pod on restore**: The initial implementation creates a new Pod from a
  checkpoint rather than restoring in-place (in-place restore is deferred to a
  future KEP for preemption/suspend use cases).
- **Kubelet awareness**: The kubelet must understand checkpoint/restore
  operations to pause health checks during checkpointing, coordinate with the
  CRI runtime, and bridge restored Pods to the API server.
- **CRI API extensions**: The `CheckpointPod` and `RestorePod` CRI RPCs
  allow different container runtimes (CRIU with runc, gVisor) to implement
  checkpointing differently behind a common interface.

## How It Works

```
kubectl apply               pod-snapshot-controller             kubelet             container runtime
      |                             |                              |                       |
      |--- PodCheckpoint ---------> |                              |                       |
      |                             |--- POST /checkpoint -------> |                       |
      |                             |                              |--- CheckpointPod ---> |
      |                             |                              |                       |-- checkpoint
      |                             |<-- checkpoint path --------- |                       |
      |                             |                              |                       |
      |--- PodRestore ------------> |                              |                       |
      |                             |--- create Pod (API server)   |                       |
      |                             |--- POST /restore ----------> |                       |
      |                             |                              |--- RestorePod ------> |
      |                             |                              |                       |-- restore
      |                             |<-- sandbox ID -------------- |                       |
```

1. User creates a **PodCheckpoint** referencing a running Pod.
2. The controller calls the kubelet checkpoint API via the API server node
   proxy. The checkpoint is saved to disk on the node. The Pod keeps running
   (non-disruptive).
3. User deletes the Pod and creates a **PodRestore** referencing the
   checkpoint.
4. The controller creates a new Pod in the API server, then calls the kubelet
   restore API. The container runtime restores the exact process state
   (memory, registers, file descriptors), so the application continues from
   where it left off.

## Prerequisites

- Kubernetes with the `KubeletLocalPodCheckpointRestore` feature gate enabled
- A container runtime that supports the `CheckpointPod`/`RestorePod` CRI RPCs
  (e.g., containerd with CRIU, or gVisor)
- For CRIU-based runtimes (runc, crun, youki): CRIU installed on all nodes

## Installation

Install the CRDs:

```bash
kubectl apply -f config/crd/bases/
```

Build and run the controller (out-of-cluster for development):

```bash
make build
bin/pod-snapshot-controller --kubeconfig=$HOME/.kube/config
```

Or build and deploy in-cluster:

```bash
make docker-build IMG=<registry>/pod-snapshot-controller:latest
make deploy IMG=<registry>/pod-snapshot-controller:latest
```

## Custom Resources

### PodCheckpoint

```yaml
apiVersion: checkpoint.k8s.io/v1alpha1
kind: PodCheckpoint
metadata:
  name: my-checkpoint
spec:
  sourcePodName: my-app       # Name of the running Pod to checkpoint
  timeoutSeconds: 30          # Optional; 0 = runtime default
```

Status phases: `Pending` -> `InProgress` -> `Ready` | `Failed`

### PodRestore

```yaml
apiVersion: checkpoint.k8s.io/v1alpha1
kind: PodRestore
metadata:
  name: my-restore
spec:
  checkpointName: my-checkpoint   # Name of the PodCheckpoint to restore from
```

Status phases: `Pending` -> `Restoring` -> `Completed` | `Failed`

## Example

A full walkthrough is available in [examples/](examples/). The automated script
runs the entire checkpoint/restore cycle:

```bash
./examples/run.sh
```

Or step by step:

```bash
# 1. Create a Pod with an incrementing counter
kubectl apply -f examples/counter-pod.yaml
kubectl wait --for=condition=Ready pod/counter-app

# 2. Checkpoint the Pod
kubectl apply -f examples/checkpoint.yaml
kubectl get podcheckpoint counter-checkpoint -w

# 3. Delete the Pod
kubectl delete pod counter-app

# 4. Restore from checkpoint
kubectl apply -f examples/restore.yaml
kubectl get podrestore counter-restore -w

# 5. Verify the counter continued from where it was checkpointed
kubectl logs counter-app --tail=10
```

## Development

```bash
make manifests   # Regenerate CRD manifests and RBAC
make generate    # Regenerate deepcopy methods
make build       # Build the controller binary
make test        # Run unit tests
```
