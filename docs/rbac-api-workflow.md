# RBAC & API Workflow Walkthrough: Pod Checkpoint/Restore

## API Group & Resources

Define namespace-scoped resources under `checkpoint.k8s.io/v1alpha1`:

| Resource | Kind | Subresources |
|---|---|---|
| `podcheckpoints` | `PodCheckpoint` | `/status` (both), `/finalizers` (external only) |
| `podrestores` | `PodRestore` | `/status` (both), `/finalizers` (external only) |

Namespace scoping means a PodCheckpoint in `team-a` can only target Pods in `team-a`, and a PodRestore in `team-a` can only reference PodCheckpoints in `team-a`.

---

## Architecture Overview

### Built-in Controller

Controllers run inside `kube-controller-manager` in `kube-system`. The kubelet checkpoint call goes directly to the kubelet's HTTPS endpoint.

```
User/Operator                  API Server                Controllers              Kubelet
     |                            |                          |                      |
     |-- create PodCheckpoint --->|                          |                      |
     |                            |-- watch event ---------> |                      |
     |                            |                   [podcheckpoint-controller]    |
     |                            |                          |-- GET pod (ns) ----->|
     |                            |                          |-- GET node --------->|
     |                            |                          |-- POST kubelet:10250/checkpointpod/{ns}/{pod} ->|
     |                            |<-- update /status -------|                      |
     |                            |   phase: Ready           |                      |
     |                            |   checkpointLocation: ...|                      |
     |                            |                          |                      |
     |-- create PodRestore ------>|                          |                      |
     |                            |-- watch event ---------> |                      |
     |                            |                   [podrestore-controller]       |
     |                            |                          |-- GET PodCheckpoint  |
     |                            |                          |   (must be Ready)    |
     |                            |                          |-- CREATE Pod ------->|
     |                            |                          |   spec.restoreFrom   |
     |                            |<-- update /status -------|                      |
     |                            |   phase: Completed       |                      |
```

### External Controller

Controller runs as a standalone deployment (`pod-snapshot-controller`) in `pod-snapshot-controller-system`. The kubelet checkpoint call goes through the API server's node proxy.

```
User                    API Server              Controller                         Kubelet
 |                         |                       |                                  |
 +- kubectl apply          |                       |                                  |
 |  PodCheckpoint -------+ |                       |                                  |
 |  (ns: team-a,           |                       |                                  |
 |   pod: my-app)          |                       |                                  |
 |                         |                       |                                  |
 |  RBAC check:            |                       |                                  |
 |  user needs CREATE on   |                       |                                  |
 |  podcheckpoints in      |                       |                                  |
 |  team-a                 |                       |                                  |
 |                         +-- watch event ------+  |                                  |
 |                         |                       |                                  |
 |                         |  1. Add finalizer     |                                  |
 |                         |  2. GET Pod (ns) -----+                                  |
 |                         |  3. Add finalizer to  |                                  |
 |                         |     source Pod        |                                  |
 |                         |  4. Status->InProgress|                                  |
 |                         |  5. POST via node ----+--- nodes/proxy ----------------->|
 |                         |     proxy             |   kubelet runs CRIU dump         |
 |                         |                       | <-- {"/path/chk.tar"} -----------|
 |                         |  6. Remove source     |                                  |
 |                         |     Pod finalizer     |                                  |
 |                         |  7. Status -> Ready --+                                  |
```

---

## Controller RBAC

### Built-in system:controller:podcheckpoint-controller

Runs in `kube-system`.

```
checkpoint.k8s.io  podcheckpoints         get, list, watch, update, patch
checkpoint.k8s.io  podcheckpoints/status   update, patch
                   pods                    get, list, watch
                   nodes                   get, list, watch
                   nodes/proxy             get, create
                   nodes/checkpointpod     create
```

### Built-in system:controller:podrestore-controller

```
checkpoint.k8s.io  podrestores             get, list, watch, update, patch
checkpoint.k8s.io  podrestores/status      update, patch
checkpoint.k8s.io  podcheckpoints          get, list, watch
""                 pods                    get, list, watch, create
```

### External Controller: pod-snapshot-controller ClusterRole

Deployed via `config/rbac/role.yaml` and runs in `pod-snapshot-controller-system`.

| API Group | Resource | Verbs | Why |
|-----------|----------|-------|-----|
| `checkpoint.k8s.io` | `podcheckpoints` | full CRUD + watch | Read specs, manage lifecycle |
| `checkpoint.k8s.io` | `podcheckpoints/status` | get, patch, update | Set phase, conditions, location |
| `checkpoint.k8s.io` | `podcheckpoints/finalizers` | update | Add/remove protection finalizer |
| `checkpoint.k8s.io` | `podrestores` | full CRUD + watch | Read specs, manage lifecycle |
| `checkpoint.k8s.io` | `podrestores/status` | get, patch, update | Set phase, conditions, sandboxID |
| `checkpoint.k8s.io` | `podrestores/finalizers` | update | Future use |
| `""` (core) | `pods` | get, list, watch, create, delete, update, patch | Read source Pod, add/remove finalizers, create placeholder Pod for restore |
| `""` (core) | `nodes/proxy` | create | POST through the API server's node proxy to kubelet checkpoint/restore endpoints |

### Key RBAC Differences

| Aspect | Built-in | External |
|--------|----------|----------|
| **Kubelet access** | Direct HTTPS to kubelet IP:10250; uses `nodes/proxy` + `nodes/checkpointpod` | Via API server node proxy; uses `nodes/proxy` only |
| **Pod permissions** | Separate controllers: checkpoint needs only read; restore needs create | Single controller: needs full CRUD (for finalizers, placeholder pods) |
| **Source pod protection** | Not implemented | Controller adds/removes finalizer on source pod (needs `pods.update`) |
| **Deletion protection** | Not implemented | Finalizer protocol blocks deletion while active restores exist |

---

## User-Facing RBAC

### Built-in Approach

The bootstrapped roles cover controllers only. For users or CI systems, a namespaced Role can be created as follows:

```yaml
# Role: checkpoint-operator (bind per-namespace)
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: team-a
  name: checkpoint-operator
rules:
- apiGroups: ["checkpoint.k8s.io"]
  resources: ["podcheckpoints"]
  verbs: ["create", "get", "list", "watch", "delete"]
- apiGroups: ["checkpoint.k8s.io"]
  resources: ["podcheckpoints/status"]
  verbs: ["get"]              # read-only on status
- apiGroups: ["checkpoint.k8s.io"]
  resources: ["podrestores"]
  verbs: ["create", "get", "list", "watch", "delete"]
- apiGroups: ["checkpoint.k8s.io"]
  resources: ["podrestores/status"]
  verbs: ["get"]
```

### External Controller

Provides three pre-defined ClusterRoles (bindable per-namespace via RoleBinding):

| Role | Verbs on `podcheckpoints` / `podrestores` | Verbs on `*/status` | Use case |
|------|------------------------------------------|---------------------|----------|
| **viewer** | get, list, watch | get | Monitoring, read-only dashboards |
| **editor** | get, list, watch, create, update, patch, delete | get | Developers triggering checkpoint/restore |
| **admin** | `*` (all) | get | Cluster admins delegating RBAC |

**Example**

Grant Alice editor access only in `team-a`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: checkpoint-editor
  namespace: team-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: pod-snapshot-controller-podcheckpoint-editor-role
subjects:
- kind: User
  name: alice
```

In both approaches, users do not need `nodes/proxy`, `nodes/checkpointpod`, or direct pod-create permission.
The controllers handle those privileged operations.

---

## End-to-End Checkpoint Workflow

### Built-in Controller

1. User creates `PodCheckpoint` in `team-a` with `spec.sourcePodName: my-app-pod`
2. Controller validates pod exists and is `Running` in `team-a`
3. Resolves pod's `spec.nodeName`, looks up the node's internal IP
4. POSTs directly to `https://<nodeIP>:10250/checkpointpod/team-a/my-app-pod`
5. Updates status: `phase: Ready`, `checkpointLocation: /var/lib/kubelet/checkpoints/...`

### External Controller

1. User creates `PodCheckpoint` in `team-a` with `spec.sourcePodName: my-app`
2. Controller adds finalizer `checkpoint.k8s.io/podcheckpoint-protection`
3. GETs Pod in `team-a`, validates `Running` and `nodeName != ""`
4. Adds finalizer `checkpoint.k8s.io/source-pod-protection` to source Pod (prevents deletion during CRIU)
5. Sets status to `InProgress`
6. POSTs via API server node proxy: `/api/v1/nodes/{node}/proxy/checkpoint/team-a/my-app`
7. Removes source Pod finalizer
8. Sets status to `Ready` with `checkpointLocation`, `nodeName`, and `containers[]`

**Failure path** (external): If step 6 fails, the controller removes the source Pod finalizer and sets status to `Failed`. The kubelet checkpoint API is idempotent, so `InProgress` is retried on controller restart.

---

## End-to-End Restore Workflow

### Built-in (Single Step)

1. User creates `PodRestore` in `team-a` with `spec.checkpointName` and `spec.podTemplate`
2. Controller fetches `PodCheckpoint` in `team-a`, verifies `phase: Ready`
3. Parses `checkpointLocation` to get the archive path
4. Creates a new Pod with `spec.restoreFrom` set to the checkpoint path
5. Kubelet sees `restoreFrom` during SyncPod and calls `restorePodSandbox()` instead of `createPodSandbox()` (pod creation = restore)
6. Updates status: `phase: Completed`, `restoredPodName: my-restore-xyz`

### External (Two-Phase)

1. User creates `PodRestore` in `team-a` with `spec.checkpointName`
2. Controller fetches `PodCheckpoint` in `team-a`, verifies `phase: Ready`, `nodeName != ""`, `location != ""`
3. Sets status to `Restoring`
4. Creates a placeholder Pod in `team-a` with `spec.nodeName` pinned to the checkpoint's node and annotation `checkpoint.k8s.io/restored-from`
5. Kubelet sees annotation and **skips SyncPod** for this pod; CNI plugins set up networking
6. Controller polls every 2s for PodIP or ContainerStatuses (sandbox readiness)
7. POSTs via API server node proxy: `/api/v1/nodes/{node}/proxy/restore/team-a/chk.tar`
8. Kubelet reads saved `PodSandboxConfig` from `pod-config.json`, patches UID/cgroup/logdir, calls CRI `RestorePod`
9. Sets status to `Completed` with `restoredSandboxId`

---

## Deletion Protection (External Only)

The external controller uses a finalizer-based protocol to prevent data loss:

```
DELETE PodCheckpoint
       |
       v
  Has finalizer?  --no-->  done
       | yes
       v
  List PodRestores in same namespace
       |
       v
  Any active restore referencing
  this checkpoint?  --yes-->  requeue (5s)
       | no                    (block deletion until
       v                        restore completes/fails)
  Clean up source Pod finalizer
  if stuck in InProgress
       |
       v
  deletionPolicy == Delete?
       | yes
       v
  Log intent to clean node data       <- TODO: kubelet deletion endpoint
       |                                 doesn't exist yet
       v
  Remove finalizer -> object deleted
```

The built-in approach does not implement deletion protection.

---

## Namespace Security Boundaries

| Concern | Built-in | External |
|---|---|---|
| **Cross-namespace checkpoint** | Blocked: controller scopes pod lookup to PodCheckpoint's namespace | Blocked — `ObjectKey{Namespace: checkpoint.Namespace}` |
| **Cross-namespace restore** | Blocked: controller scopes PodCheckpoint lookup to PodRestore's namespace | Blocked: `ObjectKey{Namespace: restore.Namespace}` |
| **User escalation to node access** | Blocked: only controllers hold `nodes/proxy` and `nodes/checkpointpod` | Blocked: only controller SA holds `nodes/proxy`; never granted to users |
| **Spec mutation after create** | Blocked: `sourcePodName` and `checkpointName` are immutable | Same |
| **User writing status** | Blocked: REST storage strategy strips status on spec updates | Same (CRD subresource) |
| **Controller overwriting spec** | Blocked: status strategy strips spec on status updates | Same |
| **Deletion during active restore** | Not protected | Blocked: finalizer protocol prevents checkpoint deletion while restores are active |
| **Source pod deletion during CRIU** | Not protected | Blocked: source pod finalizer prevents deletion during checkpoint operation |
