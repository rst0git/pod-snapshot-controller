# Pod-Level Checkpoint/Restore: Architecture and Design

## Table of Contents

- [Overview](#overview)
- [Architecture](#architecture)
  - [Three-Layer Design](#three-layer-design)
  - [Component Diagram](#component-diagram)
- [Layer 1: CRI API Extensions](#layer-1-cri-api-extensions)
  - [CheckpointPod RPC](#checkpointpod-rpc)
  - [RestorePod RPC](#restorepod-rpc)
  - [Containerd Implementation](#containerd-implementation)
- [Layer 2: Kubelet Endpoints](#layer-2-kubelet-endpoints)
  - [Checkpoint Endpoint](#checkpoint-endpoint)
  - [Restore Endpoint](#restore-endpoint)
  - [Kubelet Internals](#kubelet-internals)
- [Layer 3: CRD and Controller](#layer-3-crd-and-controller)
  - [API Objects](#api-objects)
  - [Pod-Snapshot-Controller](#pod-snapshot-controller)
  - [Checkpoint Reconciliation](#checkpoint-reconciliation)
  - [Restore Reconciliation](#restore-reconciliation)
- [Request Lifecycle](#request-lifecycle)
  - [Checkpoint Flow](#checkpoint-flow)
  - [Restore Flow](#restore-flow)
- [Key Design Decisions](#key-design-decisions)
  - [Why Three Layers](#why-three-layers)
  - [Core API vs CRD Boundary](#core-api-vs-crd-boundary)
  - [New Pod UID on Restore](#new-pod-uid-on-restore)
  - [Placeholder Pod Pattern](#placeholder-pod-pattern)
  - [Finalizer-Based Lifecycle Management](#finalizer-based-lifecycle-management)
  - [Poll-Based Sandbox Readiness](#poll-based-sandbox-readiness)
  - [Idempotent Reconciliation and Crash Recovery](#idempotent-reconciliation-and-crash-recovery)
  - [Deletion Policy](#deletion-policy)
  - [Immutable Spec Fields](#immutable-spec-fields)
- [Checkpoint Data Format](#checkpoint-data-format)
- [Security](#security)
- [Precedents: Lessons from VolumeSnapshot and JobSet](#precedents-lessons-from-volumesnapshot-and-jobset)
- [Limitations and Future Work](#limitations-and-future-work)

---

## Overview

Pod-level Checkpoint/Restore enables capturing the complete runtime state of a
running Kubernetes Pod (all containers, in-memory state, process trees, file
descriptors) and later restoring it into a new Pod. The mechanism is
transparent to the application: processes resume exactly where they left off.

The implementation spans five repositories:

| Repository | Role |
|------------|------|
| `kubernetes` | CRI proto definitions, kubelet endpoints, feature gate |
| `containerd` | CRI server implementation (CheckpointPod/RestorePod) |
| `cri-tools` | `crictl checkpointpod` / `crictl restorepod` CLI |
| `pod-snapshot-controller` | CRDs (PodCheckpoint, PodRestore) and reconciliation controller |
| `enhancements` | KEP-5823 specification |

Feature gate: `KubeletLocalPodCheckpointRestore` (kubelet component, alpha).

---

## Architecture

### Three-Layer Design

```
┌─────────────────────────────────────────────────────────────────────┐
│  Layer 3: Kubernetes API Objects (CRD)                              │
│                                                                     │
│  PodCheckpoint ──────────────────────────── PodRestore              │
│  checkpoint.k8s.io/v1alpha1                checkpoint.k8s.io/v1alph │
│                                                                     │
│  pod-snapshot-controller                                            │
│  (watches CRDs, calls kubelet via API server node proxy)            │
├─────────────────────────────────────────────────────────────────────┤
│  Layer 2: Kubelet HTTP Endpoints                                    │
│                                                                     │
│  POST /checkpoint/{namespace}/{pod}[?timeout=N]                     │
│  POST /restore/{namespace}/{checkpointName}                         │
│                                                                     │
│  (resolves sandbox ID, manages probe suspension, delegates to CRI)  │
├─────────────────────────────────────────────────────────────────────┤
│  Layer 1: CRI API (Container Runtime Interface)                     │
│                                                                     │
│  CheckpointPod(pod_sandbox_id, path, timeout, options)              │
│  RestorePod(path, config, container_configs, options)               │
│                                                                     │
│  containerd / CRI-O  ->  runc/crun + CRIU  |  gVisor                 │
└─────────────────────────────────────────────────────────────────────┘
```

Each layer has a distinct responsibility. **Layer 1 (CRI)** performs the
actual checkpoint/restore through the container runtime and CRIU. It knows
nothing about Kubernetes objects. **Layer 2 (Kubelet)** bridges Kubernetes
Pod identity to CRI sandbox identity, managing probe suspension, UID
assignment, and cgroup paths. **Layer 3 (CRD)** provides the declarative,
user-facing API. It orchestrates the lifecycle and communicates with the
kubelet through the API server's node proxy.

### Component Diagram

```
                  ┌──────────────┐
                  │   User /     │
                  │   kubectl    │
                  └──────┬───────┘
                         │ create PodCheckpoint / PodRestore
                         v
                  ┌──────────────┐
                  │  API Server  │
                  └──────┬───────┘
                         │
              ┌──────────┴──────────┐
              v                     v
   ┌────────────────────┐  ┌───────────────────┐
   │  pod-snapshot-     │  │  kubelet          │
   │  controller        │  │                   │
   │                    │  │  /checkpoint/...  │
   │  Reconcile loop:   │──│  /restore/...     │
   │  CRD -> node proxy │  │                   │
   └────────────────────┘  └────────┬──────────┘
                                    │ CRI gRPC
                                    v
                           ┌───────────────────┐
                           │  containerd       │
                           │                   │
                           │  CheckpointPod()  │
                           │  RestorePod()     │
                           └────────┬──────────┘
                                    │ OCI runtime
                                    v
                           ┌───────────────────┐
                           │  runc + CRIU      │
                           │                   │
                           │  freeze -> dump ->│
                           │  unfreeze         │
                           └───────────────────┘
```

---

## Layer 1: CRI API Extensions

Two new RPCs are added to `RuntimeService` in `api.proto`:

### CheckpointPod RPC

```protobuf
rpc CheckpointPod(CheckpointPodRequest) returns (CheckpointPodResponse) {}

message CheckpointPodRequest {
    string pod_sandbox_id = 1;      // CRI sandbox ID to checkpoint
    string path = 2;                // Directory to save checkpoint data
    int64 timeout = 3;              // Timeout in seconds (0 = runtime default)
    map<string, string> options = 4;
}

message CheckpointPodResponse {}
```

### RestorePod RPC

```protobuf
rpc RestorePod(RestorePodRequest) returns (RestorePodResponse) {}

message RestorePodRequest {
    string path = 1;                           // Checkpoint directory
    PodSandboxConfig config = 2;               // Optional config override
    int64 timeout = 3;                         // Timeout in seconds
    map<string, string> options = 4;
    repeated ContainerConfig container_configs = 5; // Mount configurations
}

message RestorePodResponse {
    string pod_sandbox_id = 1;                 // Restored sandbox ID
}
```

### Containerd Implementation

The containerd implementation (`sandbox_checkpoint_linux.go`) orchestrates
checkpoint/restore at the Pod level by iterating over containers:

**Checkpoint steps:**

1. Validate sandbox exists and path is provided.
2. Serialize `PodSandboxConfig` to `pod-config.json` in the checkpoint directory.
3. For each container belonging to the sandbox that is in `RUNNING` state
   (skipping created/exited containers), call the existing
   `CheckpointContainer` RPC, saving as `container-{name}.tar`.
4. Write `checkpoint-manifest.json` listing all checkpointed containers and
   the sandbox ID.

**Restore steps:**

1. Load `pod-config.json` from checkpoint (or use provided config).
2. Generate new UID if not provided by caller (avoids naming conflicts).
3. Update log directory and cgroup parent to reflect new UID.
4. Search for an existing sandbox matching the Pod metadata. If one is
   found, stop and remove any running containers to prepare for restore.
   If not, create a new sandbox via `RunPodSandbox`.
5. Load container list from `checkpoint-manifest.json`.
6. For each container, extract bind mount information from `spec.dump` in
   the checkpoint archive, ensure host paths for mounts exist (creating
   directories/files as needed), create the container with the checkpoint
   archive as the image source, and start it (which triggers CRIU restore).
7. Return the sandbox ID.

**Checkpoint directory layout:**

```
snapshot-{podName}_{namespace}-{timestamp}/
├── pod-config.json                    # Serialized PodSandboxConfig (JSON)
├── checkpoint-manifest.json           # Container list + sandbox ID
└── container-{name}.tar               # CRIU checkpoint archive per container
    ├── spec.dump                      #   OCI spec with mount information
    ├── inventory.img                  #   CRIU process inventory
    ├── core-*.img                     #   Process state
    ├── mm-*.img                       #   Memory mappings
    ├── pages-*.img                    #   Memory pages
    ├── fs-*.img                       #   File system state
    └── ...                            #   Additional CRIU images
```

**Platform support:** Linux only. Non-Linux platforms return `Unimplemented`.

---

## Layer 2: Kubelet Endpoints

### Checkpoint Endpoint

```
POST /checkpoint/{podNamespace}/{podName}[?timeout={seconds}]
Response: {"items": ["/var/lib/kubelet/pod-snapshots/snapshot-{pod}_{ns}-{timestamp}"]}
```

**Kubelet `CheckpointPod` implementation:**

1. Validate Pod exists and is in `Running` phase.
2. Generate snapshot path: `{kubelet-root}/pod-snapshots/snapshot-{podFullName}-{RFC3339-timestamp}`.
3. Look up CRI sandbox ID from Pod UID (Pod UID is not the same as CRI
   sandbox ID) and verify the sandbox is in `READY` state.
4. Suspend liveness/readiness probes (frozen cgroups would cause timeouts).
5. Call `CRI CheckpointPod(sandbox_id, path, timeout)`.
6. Resume probes and return checkpoint path in JSON response.

### Restore Endpoint

```
POST /restore/{podNamespace}/{checkpointName}
Response: {"podSandboxId": "{containerd-sandbox-id}"}
```

**Kubelet `RestorePod` implementation:**

1. Validate checkpoint name (reject path traversal: `/`, `..`), resolve the
   full path within `/var/lib/kubelet/pod-snapshots/`, and verify the
   resolved path stays within the snapshot directory using `filepath.Abs`
   and prefix comparison.
2. Read `pod-config.json` from checkpoint to get `PodSandboxConfig`.
3. Look up existing Pod object from the API server (created by the
   controller before calling restore, needed for CNI).
4. Update the config for the new Pod: assign the Pod UID from the API
   server Pod, update `CgroupParent` to point to the new cgroup hierarchy,
   and update `LogDirectory` to match the new Pod UID.
5. Build `ContainerConfig` entries from Pod spec for mount configurations.
6. Call `CRI RestorePod(path, config, container_configs)`.
7. Return restored sandbox ID.

### Kubelet Internals

**SyncPod bypass for restored Pods:** The kubelet's normal `SyncPod` loop
would detect the restored containers as not matching the expected state and
kill them. To prevent this, the kubelet checks for the annotation
`checkpoint.k8s.io/restored-from` on the Pod. When present, it skips
container hash validation (restored containers have different hashes) and
all container lifecycle management in `SyncPod`, preventing the kubelet
from killing the restored containers.

**Feature gate:** Both endpoints return HTTP 404 when the
`KubeletLocalPodCheckpointRestore` feature gate is disabled.

---

## Layer 3: CRD and Controller

### API Objects

**API Group:** `checkpoint.k8s.io`
**Version:** `v1alpha1`

#### PodCheckpoint

```yaml
apiVersion: checkpoint.k8s.io/v1alpha1
kind: PodCheckpoint
metadata:
  name: my-checkpoint
  namespace: default
  finalizers:
  - checkpoint.k8s.io/podcheckpoint-protection    # Added by controller
spec:
  sourcePodName: my-app               # Immutable after creation
  timeoutSeconds: 30                  # Optional, 0 = runtime default
  deletionPolicy: Delete              # Delete (default) or Retain
status:
  phase: Ready                        # Pending -> InProgress -> Ready | Failed
  nodeName: node-1
  checkpointLocation: /var/lib/kubelet/pod-snapshots/snapshot-my-app_default-...
  containers:
  - name: main
    image: my-app:latest
  conditions:
  - type: Ready
    status: "True"
    reason: CheckpointCompleted
    message: Pod "my-app" checkpointed successfully
```

**Phase state machine:**

```
(created) -> Pending -> InProgress -> Ready
                  │            └───-> Failed
                  └────────────────-> Failed
```

#### PodRestore

```yaml
apiVersion: checkpoint.k8s.io/v1alpha1
kind: PodRestore
metadata:
  name: my-restore
  namespace: default
spec:
  checkpointName: my-checkpoint       # Immutable after creation
status:
  phase: Completed                    # Pending -> Restoring -> Completed | Failed
  restoredSandboxID: 48baae67c711...
  conditions:
  - type: Ready
    status: "True"
    reason: RestoreCompleted
    message: Pod restored from checkpoint "my-checkpoint" (sandbox: 48baae67...)
```

**Phase state machine:**

```
(created) -> Pending -> Restoring -> Completed
                  │           └───-> Failed
                  └───────────────-> Failed
```

### Pod-Snapshot-Controller

The controller is built on `controller-runtime` and deployed as a single
binary. It runs two reconcilers:

| Reconciler | Watches | Creates/Updates |
|------------|---------|-----------------|
| `PodCheckpointReconciler` | PodCheckpoint | Updates PodCheckpoint status, manages Pod finalizers |
| `PodRestoreReconciler` | PodRestore | Creates placeholder Pod, updates PodRestore status |

The controller communicates with the kubelet exclusively through the API
server's node proxy endpoint (`/api/v1/nodes/{node}/proxy/...`). This
requires `nodes/proxy` RBAC permission, but benefits from API server
authentication and authorization, and works in clusters where kubelets
are not directly reachable.

### Checkpoint Reconciliation

```
                    PodCheckpointReconciler.Reconcile()
                                │
                    ┌───────────┴───────────┐
                    │ Deletion in progress? │
                    └───────────┬───────────┘
                          yes / │ \ no
                              │    │
               handleDeletion()    │
               - Check for active  │
                 PodRestores       │
               - Clean up source   │
                 Pod finalizer     │
               - Log cleanup       │
                 intent            │
               - Remove finalizer  │
                                   v
                    ┌──────────────────────┐
                    │ Add protection       │
                    │ finalizer            │
                    └──────────┬───────────┘
                               v
                    ┌──────────────────────┐
                    │ Phase == Ready or    │--yes---> return (skip)
                    │ Failed?              │
                    └──────────┬───────────┘
                          no   │
                               v
                    ┌──────────────────────┐
                    │ Phase == ""?         │--yes---> Set Pending + Unknown condition
                    └──────────┬───────────┘
                          no   │
                               v
                    ┌──────────────────────┐
                    │ Look up source Pod   │
                    │ - Must exist         │
                    │ - Must be Running    │
                    │ - Must have node     │
                    └──────────┬───────────┘
                               v
                    ┌──────────────────────┐
                    │ Add source Pod       │
                    │ protection finalizer │
                    └──────────┬───────────┘
                               v
                    ┌──────────────────────┐
                    │ Set InProgress       │
                    │ + condition          │
                    └──────────┬───────────┘
                               v
                    ┌──────────────────────┐
                    │ POST /checkpoint/... │
                    │ via node proxy       │
                    └──────────┬───────────┘
                         ok  / │ \ error
                              │    │
                              │    └──-> Remove source Pod finalizer
                              │        Set Failed + condition
                              v
                    ┌──────────────────────┐
                    │ Remove source Pod    │
                    │ finalizer            │
                    │ Set Ready            │
                    │ Store location +     │
                    │ container info       │
                    └──────────────────────┘
```

### Restore Reconciliation

```
                    PodRestoreReconciler.Reconcile()
                                │
                    ┌───────────┴───────────┐
                    │ Phase == Completed or │--yes---> return (skip)
                    │ Failed?               │
                    └───────────┬───────────┘
                          no   │
                               v
                    ┌──────────────────────┐
                    │ Phase == ""?         │--yes---> Set Pending + Unknown condition
                    └──────────┬───────────┘
                          no   │
                               v
                    ┌──────────────────────┐
                    │ Get PodCheckpoint    │
                    │ - Must exist         │
                    │ - Must be Ready      │
                    └──────────┬───────────┘
                    not ready / │ \ ready
                              │    │
               requeue 5s     │    v
                    ┌──────────────────────┐
                    │ Set Restoring        │
                    │ + condition          │
                    └──────────┬───────────┘
                               v
                    ┌──────────────────────┐
                    │ ensurePodForRestore()│
                    │ - Create placeholder │
                    │   Pod in API server  │
                    │ - Annotated with     │
                    │   restored-from      │
                    └──────────┬───────────┘
                               v
                    ┌──────────────────────┐
                    │ isPodSandboxReady()  │
                    │ - Check PodIP        │
                    │ - Check container    │--not ready---> requeue 2s
                    │   statuses           │
                    └──────────┬───────────┘
                          ready│
                               v
                    ┌──────────────────────┐
                    │ POST /restore/...    │
                    │ via node proxy       │
                    └──────────┬───────────┘
                         ok  / │ \ error
                              │    │
                              │    └──-> Set Failed + condition
                              v
                    ┌──────────────────────┐
                    │ Set Completed        │
                    │ Store sandbox ID     │
                    └──────────────────────┘
```

---

## Request Lifecycle

### Checkpoint Flow

```
 User                Controller              API Server    Kubelet        containerd       CRIU
  │                      │                       │            │               │              │
  │ create PodCheckpoint │                       │            │               │              │
  │─────────────────────>│                       │            │               │              │
  │                      │ GET Pod               │            │               │              │
  │                      │──────────────────────>│            │               │              │
  │                      │<──────────────────────│            │               │              │
  │                      │                       │            │               │              │
  │                      │ add source Pod        │            │               │              │
  │                      │ finalizer             │            │               │              │
  │                      │──────────────────────>│            │               │              │
  │                      │                       │            │               │              │
  │                      │ set InProgress        │            │               │              │
  │                      │──────────────────────>│            │               │              │
  │                      │                       │            │               │              │
  │                      │ POST /nodes/{n}/proxy/checkpoint/{ns}/{pod}        │              │
  │                      │──────────────────────────────────> │               │              │
  │                      │                       │            │ suspend       │              │
  │                      │                       │            │ probes        │              │
  │                      │                       │            │               │              │
  │                      │                       │            │ CheckpointPod │              │
  │                      │                       │            │──────────────>│              │
  │                      │                       │            │               │ save config  │
  │                      │                       │            │               │──────────────│
  │                      │                       │            │               │              │
  │                      │                       │            │               │ per container:
  │                      │                       │            │               │ freeze cgroup│
  │                      │                       │            │               │─────────────>│
  │                      │                       │            │               │ CRIU dump    │
  │                      │                       │            │               │<─────────────│
  │                      │                       │            │               │ unfreeze     │
  │                      │                       │            │               │              │
  │                      │                       │            │<──────────────│              │
  │                      │                       │            │ resume probes │              │
  │                      │<───────────────────────────────────│               │              │
  │                      │                       │            │               │              │
  │                      │ remove source Pod     │            │               │              │
  │                      │ finalizer             │            │               │              │
  │                      │──────────────────────>│            │               │              │
  │                      │                       │            │               │              │
  │                      │ set Ready +           │            │               │              │
  │                      │ checkpoint location   │            │               │              │
  │                      │──────────────────────>│            │               │              │
```

### Restore Flow

```
 User                Controller              API Server    Kubelet        containerd       CRIU
  │                      │                       │            │               │              │
  │ create PodRestore    │                       │            │               │              │
  │─────────────────────>│                       │            │               │              │
  │                      │ GET PodCheckpoint     │            │               │              │
  │                      │──────────────────────>│            │               │              │
  │                      │<─ phase: Ready ───────│            │               │              │
  │                      │                       │            │               │              │
  │                      │ set Restoring         │            │               │              │
  │                      │──────────────────────>│            │               │              │
  │                      │                       │            │               │              │
  │                      │ create placeholder Pod│            │               │              │
  │                      │ (nodeName + annotation)            │               │              │
  │                      │──────────────────────>│            │               │              │
  │                      │                       │            │               │              │
  │                      │                       │ kubelet    │               │              │
  │                      │                       │ syncs Pod  │               │              │
  │                      │                       │───────────>│               │              │
  │                      │                       │            │ RunPodSandbox │              │
  │                      │                       │            │──────────────>│              │
  │                      │                       │            │ CNI setup     │              │
  │                      │                       │            │<──────────────│              │
  │                      │                       │            │               │              │
  │                      │ poll: PodIP set?      │            │               │              │
  │                      │──────────────────────>│            │               │              │
  │                      │<─ yes ────────────────│            │               │              │
  │                      │                       │            │               │              │
  │                      │ POST /nodes/{n}/proxy/restore/{ns}/{checkpointName}│              │
  │                      │───────────────────────────────────>│               │              │
  │                      │                       │            │ read config   │              │
  │                      │                       │            │ update UID    │              │
  │                      │                       │            │ update cgroup │              │
  │                      │                       │            │               │              │
  │                      │                       │            │ RestorePod    │              │
  │                      │                       │            │──────────────>│              │
  │                      │                       │            │               │ find sandbox │
  │                      │                       │            │               │ stop old ctrs│
  │                      │                       │            │               │              │
  │                      │                       │            │               │ per container:
  │                      │                       │            │               │ create w/    │
  │                      │                       │            │               │ checkpoint   │
  │                      │                       │            │               │ start        │
  │                      │                       │            │               │─────────────>│
  │                      │                       │            │               │ CRIU restore │
  │                      │                       │            │               │<─────────────│
  │                      │                       │            │               │              │
  │                      │                       │            │<──────────────│              │
  │                      │<───────────────────────────────────│               │              │
  │                      │                       │            │               │              │
  │                      │ set Completed +       │            │               │              │
  │                      │ sandbox ID            │            │               │              │
  │                      │──────────────────────>│            │               │              │
```

---

## Key Design Decisions

### Why Three Layers

The three-layer architecture separates concerns so that each layer can evolve
independently:

| Layer | Changes with | Release cycle |
|-------|-------------|---------------|
| CRI protos | Container runtime releases | Independent of K8s |
| Kubelet endpoints | K8s minor releases | Feature-gated, per-node |
| CRDs + controller | Controller releases | Fully independent |

This follows the pattern established by CSI (Container Storage Interface):
core Kubernetes defines the interface, external components implement it.

### Core API vs CRD Boundary

**What is in core Kubernetes (feature-gated):** The CRI proto extensions
(`CheckpointPod`, `RestorePod`) are required for any runtime to support the
feature. The kubelet HTTP endpoints are node-local and translate Pod identity
to CRI calls. The feature gate `KubeletLocalPodCheckpointRestore` controls
endpoint availability.

**What is a CRD (external controller):** `PodCheckpoint` and `PodRestore`
provide the user-facing declarative API. The `pod-snapshot-controller`
contains all orchestration logic.

**Rationale (following Kubernetes precedent):**

| Precedent | Core API | External (CRD) |
|-----------|----------|-----------------|
| Volume Snapshots | `PVC.spec.dataSource` | `VolumeSnapshot`, `VolumeSnapshotContent`, `VolumeSnapshotClass` |
| JobSet | `Job` (batch/v1) | `JobSet` (jobset.x-k8s.io) |
| Pod Checkpoint | Kubelet endpoints, CRI RPCs | `PodCheckpoint`, `PodRestore` |

The pattern is: **core API provides primitives; CRDs provide orchestration**.
CRDs iterate faster (no K8s release coupling), can be alpha without
affecting core stability, and only need to be deployed in clusters that use
the feature.

### New Pod UID on Restore

The restored Pod gets a new UID rather than reusing the original. This avoids
conflicts with existing Pod objects (the original may still exist) and
prevents misbehavior in controllers like ReplicaSets and StatefulSets that
use UIDs for caching. It also works with the normal kubelet sync path without
special cases.

Same-UID restore (needed for live migration and suspend/resume) is deferred
to a future KEP because it modifies Pod lifecycle semantics with deep
ecosystem implications.

### Placeholder Pod Pattern

Before calling the kubelet restore endpoint, the controller creates a
"placeholder" Pod in the API server. This is necessary for three reasons.
First, network plugins like Calico query the API server for Pod metadata
during sandbox network setup, and CNI fails if the Pod doesn't exist.
Second, kubelets can only create mirror pods, not arbitrary pods, so the
controller must create the Pod through the API server. Third, the kubelet's
`RestorePod` reads the placeholder Pod's UID and uses it for the restored
sandbox, ensuring the kubelet's internal state matches the API server.

The placeholder Pod is created with `spec.nodeName` set to the checkpoint's
node (bypassing the scheduler), container names and images from the
checkpoint's stored container info, and the annotation
`checkpoint.k8s.io/restored-from: {checkpointName}` which tells the kubelet
to skip hash checking and SyncPod lifecycle for this Pod.

**What happens to containers started by the kubelet before RestorePod:**
The kubelet may start the placeholder Pod's containers normally before the
controller calls `RestorePod`. Containerd's `RestorePod` implementation
detects the existing sandbox, stops any running containers, and replaces
them with CRIU-restored processes.

### Finalizer-Based Lifecycle Management

Following the external-snapshotter pattern, the controller uses finalizers
for lifecycle protection:

The **`checkpoint.k8s.io/podcheckpoint-protection`** finalizer is added to
every PodCheckpoint on first reconciliation. When the PodCheckpoint is
deleted, the controller checks for active (non-terminal) PodRestores
referencing it and defers deletion if any exist (requeueing every 5s). If
the deletion policy is `Delete`, it logs the checkpoint location that should
be cleaned up on the node. (Actual node-side cleanup requires a kubelet
deletion endpoint, planned for post-alpha.) It also cleans up orphaned
source Pod finalizers if the checkpoint was in `InProgress` when deleted,
which handles the controller crash scenario.

The **`checkpoint.k8s.io/source-pod-protection`** finalizer is added to the
source Pod before the checkpoint operation starts and removed after it
completes (whether successful or not). This prevents accidental deletion of
the source Pod while CRIU is checkpointing its processes. If the controller
crashes during checkpoint, the PodCheckpoint deletion handler removes this
finalizer.

### Poll-Based Sandbox Readiness

The restore controller must wait for the kubelet to set up the placeholder
Pod's sandbox (with networking) before calling `RestorePod`. It uses
poll-based readiness checking:

```go
func isPodSandboxReady(pod) bool {
    return pod.Status.PodIP != "" || len(pod.Status.ContainerStatuses) > 0
}
```

If not ready, the reconciler returns `RequeueAfter: 2 * time.Second`,
allowing the controller-runtime work queue to handle other items while
waiting. This is consistent with how JobSet and other controllers handle
waiting conditions.

### Idempotent Reconciliation and Crash Recovery

Only truly terminal states (`Ready`/`Failed` for checkpoint,
`Completed`/`Failed` for restore) cause the reconciler to skip processing.
The intermediate states (`InProgress`, `Restoring`) are **not** treated as
terminal. If the controller crashes while a checkpoint or restore is in
progress, it must be able to retry on restart. This works because the
kubelet checkpoint/restore APIs are idempotent: re-checkpointing produces
a new checkpoint, and re-restoring reuses the existing sandbox. Without
this, a crash during `InProgress` would leave the operation stuck forever
with no way to recover except manual status patching.

### Deletion Policy

Following the `VolumeSnapshotContent.spec.deletionPolicy` pattern, the
`PodCheckpoint` spec includes a `deletionPolicy` field. When set to
`Delete` (the default), checkpoint data on the node should be cleaned up
when the `PodCheckpoint` object is deleted. When set to `Retain`, checkpoint
data is preserved on the node even after the object is deleted.

For alpha, the controller logs cleanup intent but does not perform actual
node-side file deletion (a kubelet deletion endpoint does not yet exist).

### Immutable Spec Fields

Both `spec.sourcePodName` (PodCheckpoint) and `spec.checkpointName`
(PodRestore) are immutable after creation, enforced via kubebuilder
`XValidation` rules:

```go
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sourcePodName is immutable"
```

This prevents confusion from changing the target after an operation has
started or completed, consistent with how `VolumeSnapshot.spec.source` is
immutable in the external-snapshotter.

---

## Checkpoint Data Format

The checkpoint data is stored on the node's filesystem at
`/var/lib/kubelet/pod-snapshots/`. Each checkpoint is a directory containing:

| File | Contents | Producer |
|------|----------|----------|
| `pod-config.json` | Serialized `PodSandboxConfig` (JSON) | containerd |
| `checkpoint-manifest.json` | Container names, sandbox ID | containerd |
| `container-{name}.tar` | CRIU checkpoint archive for each container | containerd -> runc -> CRIU |

Each container archive contains the `spec.dump` file (the OCI runtime spec
with mount information, used by restore to recreate bind mounts), the CRIU
image files (complete process state including memory pages, file descriptors,
process trees, namespaces, and cgroup state), and the root filesystem diff
(a writable overlay layer capturing filesystem changes since container start).

---

## Security

**Access control:** Kubelet checkpoint/restore endpoints are restricted to
users with privileged access to the kubelet API. `PodCheckpoint` and
`PodRestore` access is controlled through standard Kubernetes RBAC. The
pod-snapshot-controller requires `nodes/proxy` permission to call kubelet
endpoints through the API server proxy.

**Path traversal protection:** The kubelet restore endpoint validates
checkpoint names: rejects `/`, `..`, and verifies the resolved path stays
within the checkpoint storage directory using `filepath.Abs` and prefix
comparison.

**Sensitive data:** Checkpoint data may contain secrets, tokens, encryption
keys, and other sensitive in-memory contents. Checkpoint files must be
treated as sensitive with appropriate access controls (stored with
`0700` permissions, root-owned).

**Probe suspension:** The kubelet suspends liveness/readiness probes during
checkpointing because freezing container cgroups would cause probe
timeouts and potentially restart the Pod.

---

## Precedents: Lessons from VolumeSnapshot and JobSet

The design of the CRD layer draws heavily on two Kubernetes projects:

### From external-snapshotter (kubernetes-csi)

| Pattern | VolumeSnapshot | PodCheckpoint |
|---------|---------------|---------------|
| User-facing object | `VolumeSnapshot` (namespaced) | `PodCheckpoint` (namespaced) |
| System-facing object | `VolumeSnapshotContent` (cluster-scoped) | Not yet (alpha simplification) |
| Configuration class | `VolumeSnapshotClass` | Not yet (single "driver": CRIU via kubelet) |
| Deletion policy | `Delete` / `Retain` | `Delete` / `Retain` |
| Immutable spec | Source is immutable | `sourcePodName` / `checkpointName` immutable |
| Finalizer protection | Extensive (bound protection, PVC source protection) | Checkpoint protection, source Pod protection |
| Split controller | Common controller + sidecar per CSI driver | Single controller (one "driver") |
| Core API bridge | `PVC.spec.dataSource` references VolumeSnapshot | Annotation `checkpoint.k8s.io/restored-from` |

**Key lesson:** The two-object pattern (`VolumeSnapshot` + `VolumeSnapshotContent`)
separates user intent from storage backend details. For post-alpha, a
`PodCheckpointContent` object would enable pre-existing checkpoint import
and storage backend abstraction.

### From JobSet (kubernetes-sigs)

| Pattern | JobSet | Pod Checkpoint Controller |
|---------|--------|--------------------------|
| CRD wrapping core objects | Creates standard `Job` objects | Creates standard `Pod` objects |
| Status aggregation | Per-ReplicatedJob status | Per-container checkpoint info |
| Failure policies | Rule-based (FailJobSet, RestartJobSet, etc.) | Simple (fail on error) |
| Webhook validation | Extensive defaulting + validation | XValidation immutability rules |
| `managedBy` field | Allows Kueue to manage | Not yet (future: allow external controllers) |

**Key lesson:** CRDs that orchestrate core objects should compose with them,
not replace them. The checkpoint controller creates standard Pods and lets
the kubelet handle the runtime details, just as JobSet creates standard Jobs
and lets the Job controller handle pod management.

---

## Limitations and Future Work

### Alpha Limitations

Restore must happen on the same node where the checkpoint was taken, since
cross-node restore requires a checkpoint transport mechanism that does not
yet exist. The restored Pod always gets a new UID; in-place restore (same
UID) is deferred. Shared memory, volumes, and DRA devices are not
checkpointed. All TCP connections are closed on checkpoint, and connection
repair is deferred. Only regular containers are checkpointed (not init
containers). Deleting a PodCheckpoint with `deletionPolicy: Delete` logs
the intent but does not actually delete files on the node, since no kubelet
deletion endpoint exists yet. Finally, restored Pods are standalone and not
managed by any workload controller (no ReplicaSet, StatefulSet, etc.
ownership).

### Planned for Future Iterations

Cross-node restore will require checkpoint storage backends such as OCI
registries, S3, or PVCs. Introducing `PodCheckpointContent` and
`PodCheckpointClass` (the two-object pattern from VolumeSnapshot) would
enable storage backend abstraction and pre-existing checkpoint import.
In-place restore (same Pod UID) is needed for live migration and
suspend/resume. Checkpoint lifecycle management (quotas, retention policies,
garbage collection) and scheduling integration (checkpoint-aware preemption
via Kueue) are also planned. Distributed checkpointing through multi-Pod
coordination via criu-coordinator will support distributed applications. An
application notification mechanism will let applications detect they have
been restored (similar to gVisor's `/proc/gVisor/checkpoint`). Encryption
and compression of checkpoint data are planned as well.
