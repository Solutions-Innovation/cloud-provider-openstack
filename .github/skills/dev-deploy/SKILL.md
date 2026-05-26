---
name: dev-deploy
description: >
  Build, push, deploy, and verify the cinder-iscsi CSI plugin against a remote
  staging Kubernetes cluster. Interactive prompts at each stage allow running
  the full pipeline or any subset (build-only, deploy-only, etc.).
  Generates a kubeconfig with insecure-skip-tls-verify + token auth for
  clusters whose API server certs are signed with a local IP.
license: Apache-2.0
---

# Dev Deploy — Build, Push & Deploy CSI Plugin to Staging

## When to activate

Activate this skill when the user says any of:
- `/dev-deploy`, "deploy to staging", "push and deploy", "test on cluster"
- "build and push image", "deploy plugin", "dev test workflow"

Do NOT activate for local-only builds (use `/inner-loop` instead) or for
production releases.

## Namespace handling rule

**Consumer objects** (PVCs, Pods, test workloads, etc.) — namespace-scoped
resources that are NOT part of the CSI driver infrastructure — MUST be applied
to the **`default` namespace**, not `kube-system`. If a specific use case
requires `kube-system` for a consumer object, **ask the user for consent**
before proceeding.

**Driver infrastructure** (Deployments, DaemonSets, ServiceAccounts, Secrets,
RBAC for the CSI controller/node plugins) defaults to **`kube-system`**. Before
deploying driver infrastructure, **notify the user** that manifests will be
applied to `kube-system` and **ask for consent**.

Examples:
- PVC, Pod, nginx test → `default` namespace (no flag needed, or `-n default`)
- StorageClass, CSIDriver → cluster-scoped (no namespace)
- Controller Deployment, Node DaemonSet, RBAC, Secret → `kube-system` (ask consent)

## Path handling rule

**ALWAYS use `$HOME` instead of `~`** in all shell commands and file paths.
The tilde `~` is a shell expansion that does NOT work reliably in all contexts
(e.g., `kubectl --kubeconfig=~/.kube/config` fails because kubectl does not
expand `~`). Use `$HOME/.kube/config-staging` everywhere.

When the user provides a path containing `~`, silently replace it with `$HOME`
before using it in any command.

## Workflow

### Step 1: Gather environment (interactive)

Use `ask_questions` to collect pipeline configuration. All questions in a
single call (max 4):

**Question 1 — Pipeline stages** (multiSelect):
- Header: `Stages`
- Question: `Which stages should run?`
- Options:
  - `Build image` — compile binary + build container image
  - `Push image` — push to container registry
  - `Deploy to cluster` — apply manifests to staging K8s
  - `Verify deployment` — check pods, CSIDriver, logs
- Default: all selected

**Question 2 — Image registry + tag** (freeform):
- Header: `Image`
- Question: `Container image (registry/repo:tag)?`
- Options:
  - `docker.io/michaelbi/cinder-iscsi-csi-plugin:latest` (recommended)
  - `docker.io/michaelbi/cinder-iscsi-csi-plugin:<git-short-sha>`
  - `ghcr.io/solutions-innovation/cinder-iscsi-csi-plugin:latest`
- Parse the response into `REGISTRY_REPO` and `TAG` components.

**Question 3 — Kubeconfig** (freeform):
- Header: `Kubeconfig`
- Question: `Path to staging kubeconfig? Leave empty to generate one.`
- Options:
  - `$HOME/.kube/config-staging` (recommended)
  - `Use current KUBECONFIG`
- If the user provides an existing path AND the file exists, skip generation.
- If empty or file does not exist, proceed to kubeconfig generation (Step 2).
- **Important:** If the user provides a path with `~`, replace it with `$HOME`
  before using in any command (see Path handling rule above).

**Question 4 — K8s API server** (only if generating kubeconfig, freeform):
- Header: `API Server`
- Question: `Kubernetes API server URL?`
- Options:
  - `https://69.167.148.57:6443` (recommended)

### Step 2: Generate kubeconfig (if needed)

If Step 1 determined a kubeconfig needs to be generated:

1. **Ask how to obtain a token** using `ask_questions`:
   - Header: `Token`
   - Question: `How would you like to authenticate to the staging cluster?`
   - Options:
     - `I have a token ready — let me paste it` (recommended)
     - `Create a new ServiceAccount + token for me`
   - allowFreeformInput: true

2. **If "Create a new ServiceAccount + token":**

   This requires an **existing** kubeconfig with cluster-admin access (e.g.,
   the default `~/.kube/config` or `admin.conf` from the cluster). Ask:

   - Header: `Admin KC`
   - Question: `Path to an existing admin kubeconfig that can reach the cluster?
     (e.g., ~/.kube/config, /etc/kubernetes/admin.conf)`
   - allowFreeformInput: true

   Then create a dedicated ServiceAccount and long-lived token:
   ```bash
   export KUBECONFIG=<admin_kubeconfig_path>
   kubectl -n kube-system create serviceaccount csi-deployer --dry-run=client -o yaml | kubectl apply -f -
   kubectl create clusterrolebinding csi-deployer-admin \
     --clusterrole=cluster-admin \
     --serviceaccount=kube-system:csi-deployer \
     --dry-run=client -o yaml | kubectl apply -f -
   SA_TOKEN=$(kubectl -n kube-system create token csi-deployer --duration=87600h)
   echo "Token created successfully"
   ```

   If any command fails, report the error and stop.
   Capture `SA_TOKEN` from the output and use it in the kubeconfig below.

3. **If "I have a token ready":**
   - Ask for the token using `ask_questions`:
     - Header: `SA Token`
     - Question: `Paste the ServiceAccount token for the staging cluster.`
     - Freeform input, no options.
   - Set `SA_TOKEN` to the pasted value.

4. **Write the kubeconfig file** to the path from Step 1 using `run_in_terminal`:

```yaml
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: <API_SERVER_URL>
    insecure-skip-tls-verify: true
  name: staging
contexts:
- context:
    cluster: staging
    user: csi-deployer
    namespace: kube-system
  name: staging-csi-dev
current-context: staging-csi-dev
users:
- name: csi-deployer
  user:
    token: <SA_TOKEN>
```

Use `cat <<'EOF' > <kubeconfig_path>` to write it (ensure `$HOME` is used,
not `~`). Set permissions: `chmod 600 <kubeconfig_path>`.

5. **Validate connectivity**:
```bash
kubectl --kubeconfig=$HOME/.kube/config-staging get nodes
```
**Use `$HOME` not `~`** — kubectl does not expand tilde.
If this fails, report the error and **stop** — do not proceed with deploy.

### Step 3: Build plugin image (if "Build image" selected)

1. Run the Makefile target:
```bash
make build-local-image-cinder-iscsi-csi-plugin
```

2. If build fails with a Go version mismatch error (`go.mod requires go >=`,
   `toolchain`, `undefined:` on newer stdlib), activate the **go-version-fix**
   skill, then retry the build.

3. If build fails with a Docker socket permission error (`permission denied`
  while connecting to `/var/run/docker.sock`), ask the user whether to retry
  the Docker-backed commands with `sudo`. If the user provides or enables
  elevated access, rerun the build with `sudo` and continue using the same
  privilege level for later Docker commands (`docker images`, `docker tag`,
  `docker push`, `docker inspect`).

4. If build fails for other reasons, report the error and stop.

5. After successful build, find the exact local image name that was built:
```bash
docker images --format '{{.Repository}}:{{.Tag}}' | grep '^registry.k8s.io/provider-os/cinder-iscsi-csi-plugin:' | head -5
```

Choose the newest tag from the current build output (for example
`registry.k8s.io/provider-os/cinder-iscsi-csi-plugin:v1.35.0-48-ga12cfb84-dirty`).

6. Tag the image for the target registry:
```bash
docker tag <LOCAL_IMAGE_FROM_BUILD> <REGISTRY_REPO>:<TAG>
```

**Note:** The Makefile builds with repository `registry.k8s.io/provider-os` by
default, but the tag is versioned rather than always `latest`. Reuse the exact
source image discovered in the previous step.

### Step 4: Push image (if "Push image" selected)

1. **Check if already authenticated** to the target registry:
```bash
cat ~/.docker/config.json 2>/dev/null | grep -q '<registry_domain>'
```
Where `<registry_domain>` is extracted from `REGISTRY_REPO` (e.g., `docker.io`,
`ghcr.io`).

If you are using `sudo` for Docker because of socket permissions, check the
root Docker config instead:
```bash
sudo test -f /root/.docker/config.json && sudo grep -q '<registry_domain>' /root/.docker/config.json
```

2. **If not authenticated**, prompt the user:
   - Use `ask_questions` with freeform:
     - Header: `Docker Auth`
     - Question: `Not logged in to <registry_domain>. Please run:
       docker login <registry_domain>
       Then confirm when done.`
     - Options: `Done, I logged in` / `Skip push`
   - If user skips, skip this step entirely.

   If Docker commands are running under `sudo`, tell the user to log in with:
   `sudo docker login <registry_domain>`.

3. **Push the image**:
```bash
docker push <REGISTRY_REPO>:<TAG>
```

4. **Verify** the push succeeded by checking the exit code. Report the full
   image reference including digest if available:
```bash
docker inspect --format='{{index .RepoDigests 0}}' <REGISTRY_REPO>:<TAG> 2>/dev/null || echo "<REGISTRY_REPO>:<TAG>"
```

### Step 5: Patch manifests (if "Deploy to cluster" selected)

Update the image in the example plugin manifests to match the built/pushed image.
Only modify files under `examples/cinder-iscsi-csi-plugin/plugin/`:

1. In `controller.yaml`: replace the `cinder-iscsi-csi-plugin` container image
   with `<REGISTRY_REPO>:<TAG>`.
2. In `node.yaml`: replace the `cinder-iscsi-csi-plugin` container image
   with `<REGISTRY_REPO>:<TAG>`.

Use `replace_string_in_file` to make these changes. Match on the existing
`image:` line for the `cinder-iscsi-csi-plugin` container.

**Do NOT modify:**
- `manifests/cinder-iscsi-csi-plugin/` (canonical templates)
- `charts/cinder-iscsi-csi-plugin/values.yaml` (Helm chart)

### Step 6: Deploy to cluster (if "Deploy to cluster" selected)

Set `KUBECONFIG=$HOME/.kube/config-staging` (or the user's chosen path with
`$HOME` instead of `~`) for all kubectl commands in this step.

**Before applying**, notify the user and ask for consent using `ask_questions`:
- Header: `Namespace`
- Question: `CSI driver infrastructure (controller, node, RBAC, secret) will be
  deployed to kube-system. Consumer objects (PVCs, test pods) will use the
  default namespace. Proceed?`
- Options:
  - `Yes, deploy driver to kube-system` (recommended)
  - `Let me choose a different namespace`
- If the user chooses a different namespace, use that for all driver manifests.

Before applying the example manifests, detect whether the cluster already has a
cinder-iSCSI deployment for the same driver name:
```bash
kubectl get csidriver cinder-iscsi.csi.windriver.com
kubectl get deploy,daemonset -A | grep -i 'cinder-iscsi'
```

If the cluster already has controller/node workloads for this driver but under
different names (for example a Helm-managed release such as
`openstack-cinder-iscsi-csi-controllerplugin` and
`openstack-cinder-iscsi-csi-nodeplugin`), **do not apply** the example
`controller.yaml` and `node.yaml` manifests. That would create a second driver
workload set for the same `CSIDriver` and can conflict.

In that case, update the existing workloads in place instead:
```bash
kubectl -n kube-system set image deployment/<existing-controller-name> cinder-iscsi-csi-plugin=<REGISTRY_REPO>:<TAG>
kubectl -n kube-system set image daemonset/<existing-node-name> cinder-iscsi-csi-plugin=<REGISTRY_REPO>:<TAG>
kubectl -n kube-system rollout status deployment/<existing-controller-name> --timeout=180s
kubectl -n kube-system rollout status daemonset/<existing-node-name> --timeout=300s
```

If no existing cinder-iSCSI workloads are present, apply manifests in order:
```bash
export KUBECONFIG=$HOME/.kube/config-staging
kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/secret.yaml
kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/csi-driver.yaml
kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/rbac.yaml
kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/controller.yaml
kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/node.yaml
kubectl apply -f examples/cinder-iscsi-csi-plugin/storageclass.yaml
```

If any `kubectl apply` fails, report the error and **stop**.

### Step 6b: CDI StorageProfile patch (if CDI is present)

After deploying the driver and StorageClass, check if CDI is installed and offer
to apply the StorageProfile patch so DataVolumes default to `volumeMode: Block`.

1. **Detect CDI:**
```bash
kubectl --kubeconfig=$HOME/.kube/config-staging get crd storageprofiles.cdi.kubevirt.io 2>/dev/null
```

2. **If CDI is NOT detected**, skip this step silently.

3. **If CDI IS detected**, check the current StorageProfile:
```bash
kubectl --kubeconfig=$HOME/.kube/config-staging get storageprofile csi-sc-cinder-iscsi -o jsonpath='{.spec.claimPropertySets}' 2>/dev/null
```

4. **If `claimPropertySets` is already configured** (non-empty output), skip — already patched.

5. **If `claimPropertySets` is empty**, ask using `ask_questions`:
   - Header: `CDI Patch`
   - Question: `CDI is installed but the StorageProfile for csi-sc-cinder-iscsi has no
     claimPropertySets — DataVolumes will default to volumeMode: Filesystem, which this
     block-only driver rejects. Apply the StorageProfile patch (volumeMode: Block)?`
   - Options:
     - `Yes, patch StorageProfile` (recommended)
     - `No, skip`

6. **If user confirms**, apply the patch:
```bash
kubectl --kubeconfig=$HOME/.kube/config-staging apply -f manifests/cinder-iscsi-csi-plugin/cdi-storageprofile-patch.yaml
```

7. **Verify:**
```bash
kubectl --kubeconfig=$HOME/.kube/config-staging get storageprofile csi-sc-cinder-iscsi -o yaml
```
Confirm `spec.claimPropertySets` shows `volumeMode: Block`.

### Step 7: Verify deployment (if "Verify deployment" selected)

Set `KUBECONFIG=$HOME/.kube/config-staging` for all kubectl commands.

Run these checks in sequence:

1. **CSIDriver registration**:
```bash
kubectl get csidriver cinder-iscsi.csi.windriver.com
```
If not found, report and stop.

1b. **StorageClass**:
```bash
kubectl get sc csi-sc-cinder-iscsi
```
If not found, report and stop.

2. **Controller pod status**:
```bash
kubectl get pods -n kube-system -l app=csi-cinder-iscsi-controllerplugin -o wide || kubectl get pods -n kube-system -l component=controllerplugin,app=openstack-cinder-iscsi-csi -o wide
```

3. **Node pod status**:
```bash
kubectl get pods -n kube-system -l app=csi-cinder-iscsi-nodeplugin -o wide || kubectl get pods -n kube-system -l component=nodeplugin,app=openstack-cinder-iscsi-csi -o wide
```

4. **Wait for pods** (up to 90 seconds):
```bash
kubectl wait --for=condition=Ready pod -n kube-system -l app=csi-cinder-iscsi-controllerplugin --timeout=90s || kubectl wait --for=condition=Ready pod -n kube-system -l component=controllerplugin,app=openstack-cinder-iscsi-csi --timeout=90s
kubectl wait --for=condition=Ready pod -n kube-system -l app=csi-cinder-iscsi-nodeplugin --timeout=90s || kubectl wait --for=condition=Ready pod -n kube-system -l component=nodeplugin,app=openstack-cinder-iscsi-csi --timeout=90s
```

5. **If pods are NOT ready after timeout**, collect diagnostics:
```bash
kubectl describe pod -n kube-system -l app=csi-cinder-iscsi-controllerplugin | tail -30 || kubectl describe pod -n kube-system -l component=controllerplugin,app=openstack-cinder-iscsi-csi | tail -30
kubectl logs -n kube-system -l app=csi-cinder-iscsi-controllerplugin -c cinder-iscsi-csi-plugin --tail=30 || kubectl logs -n kube-system -l component=controllerplugin,app=openstack-cinder-iscsi-csi -c cinder-iscsi-csi-plugin --tail=30
kubectl describe pod -n kube-system -l app=csi-cinder-iscsi-nodeplugin | tail -30 || kubectl describe pod -n kube-system -l component=nodeplugin,app=openstack-cinder-iscsi-csi | tail -30
kubectl logs -n kube-system -l app=csi-cinder-iscsi-nodeplugin -c cinder-iscsi-csi-plugin --tail=30 || kubectl logs -n kube-system -l component=nodeplugin,app=openstack-cinder-iscsi-csi -c cinder-iscsi-csi-plugin --tail=30
```

6. **Report summary**:
   - CSIDriver: registered / not found
   - Controller: pod count, status, image
   - Node DaemonSet: desired/ready count, image
   - Any errors from logs

### Step 8: Cleanup reference (informational only)

After reporting the summary, include this teardown reference:

```
To remove the plugin from the staging cluster:
  export KUBECONFIG=$HOME/.kube/config-staging
  kubectl delete -f examples/cinder-iscsi-csi-plugin/storageclass.yaml
  kubectl delete -f examples/cinder-iscsi-csi-plugin/plugin/
```

## Edge cases

### Build fails — Go version mismatch
Activate `go-version-fix` skill (use `gvm` to switch), then retry the build.

### Build fails — other error
Report the error output and stop. Let the user fix and re-run.

### Docker socket permission denied
If Docker-backed commands fail with `permission denied` on
`/var/run/docker.sock`, ask whether to retry with `sudo`. If the user agrees,
rerun the Docker commands (`make build-local-image-cinder-iscsi-csi-plugin`
if it shells out to Docker, `docker images`, `docker tag`, `docker push`,
`docker inspect`) with `sudo` consistently for the rest of the workflow.

### Existing cinder-iSCSI deployment already present
If the cluster already has a cinder-iSCSI controller and node deployment for
`cinder-iscsi.csi.windriver.com`, do an in-place image update of the existing
workloads instead of applying the example workload manifests. This avoids
conflicts from running two driver stacks for the same `CSIDriver` name.

### Docker push fails — auth error
Prompt the user to run `docker login` manually, then retry once.

### kubectl unreachable
Report the connection error with the kubeconfig path and API server URL.
Suggest checking: network connectivity, token expiry, firewall rules.

### Pods stuck in ImagePullBackOff
Suggest verifying:
- Image was pushed successfully: `docker manifest inspect <REGISTRY_REPO>:<TAG>`
- Cluster nodes can reach the registry (check firewall/proxy)
- If using a private registry, an `imagePullSecret` may be needed

### Pods stuck in CrashLoopBackOff
Collect logs (Step 7.5) and report. Common causes:
- cloud.conf credentials invalid (check Secret base64 decode)
- Missing iSCSI packages on node (the Dockerfile should include them)
- Incorrect driver.conf configuration

## Variables reference

| Variable | Source | Example |
|----------|--------|---------|
| `REGISTRY_REPO` | Step 1 prompt | `docker.io/michaelbi/cinder-iscsi-csi-plugin` |
| `TAG` | Step 1 prompt | `latest` or `a1b2c3d` |
| `KUBECONFIG_PATH` | Step 1 prompt | `$HOME/.kube/config-staging` |
| `API_SERVER_URL` | Step 1 prompt | `https://69.167.148.57:6443` |
| `SA_TOKEN` | Step 2 prompt | `eyJhbG...` |

## Rules

- **NEVER modify** `manifests/` or Helm `charts/` — only `examples/cinder-iscsi-csi-plugin/plugin/`.
- **NEVER push to a registry without user confirmation** of the image name + tag.
- **NEVER embed tokens or passwords in skill output** — ask via freeform prompts.
- **ALWAYS validate kubeconfig connectivity** before deploying.
- **ALWAYS check Docker auth** before pushing — prompt only if needed.
- **ALWAYS report a clear summary** at the end with pod status + image in use.
- **ALWAYS apply consumer objects** (PVCs, Pods, test workloads) to the `default`
  namespace. If `kube-system` is needed for a consumer object, ask user consent.
- **ALWAYS ask consent** before deploying driver infrastructure to `kube-system`.
- If any step fails, **stop and report** — do not silently skip.
- Revert manifest image changes if deploy was not selected (to avoid dirty git state).
