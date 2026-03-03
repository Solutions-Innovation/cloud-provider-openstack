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
  - `~/.kube/config-staging` (recommended)
  - `Use current KUBECONFIG`
- If the user provides an existing path AND the file exists, skip generation.
- If empty or file does not exist, proceed to kubeconfig generation (Step 2).

**Question 4 — K8s API server** (only if generating kubeconfig, freeform):
- Header: `API Server`
- Question: `Kubernetes API server URL?`
- Options:
  - `https://69.167.148.57:6443` (recommended)

### Step 2: Generate kubeconfig (if needed)

If Step 1 determined a kubeconfig needs to be generated:

1. **Ask for the ServiceAccount token** using `ask_questions`:
   - Header: `SA Token`
   - Question: `Paste the ServiceAccount token for the staging cluster.
     To create one: kubectl -n kube-system create token admin --duration=87600h`
   - Freeform input, no options.

2. **Write the kubeconfig file** to the path from Step 1 using `run_in_terminal`:

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

Use `cat <<'EOF' > <kubeconfig_path>` to write it. Set permissions:
`chmod 600 <kubeconfig_path>`.

3. **Validate connectivity**:
```bash
kubectl --kubeconfig=<path> get nodes
```
If this fails, report the error and **stop** — do not proceed with deploy.

### Step 3: Build plugin image (if "Build image" selected)

1. Run the Makefile target:
```bash
make build-local-image-cinder-iscsi-csi-plugin
```

2. If build fails with a Go version mismatch error (`go.mod requires go >=`,
   `toolchain`, `undefined:` on newer stdlib), activate the **go-version-fix**
   skill, then retry the build.

3. If build fails for other reasons, report the error and stop.

4. After successful build, find the local image name:
```bash
docker images | grep cinder-iscsi-csi-plugin | head -5
```

5. Tag the image for the target registry:
```bash
docker tag registry.k8s.io/provider-os/cinder-iscsi-csi-plugin:latest <REGISTRY_REPO>:<TAG>
```

**Note:** The Makefile builds with registry `registry.k8s.io/provider-os` by default.
The tag step re-tags to the user's chosen registry.

### Step 4: Push image (if "Push image" selected)

1. **Check if already authenticated** to the target registry:
```bash
cat ~/.docker/config.json 2>/dev/null | grep -q '<registry_domain>'
```
Where `<registry_domain>` is extracted from `REGISTRY_REPO` (e.g., `docker.io`,
`ghcr.io`).

2. **If not authenticated**, prompt the user:
   - Use `ask_questions` with freeform:
     - Header: `Docker Auth`
     - Question: `Not logged in to <registry_domain>. Please run:
       docker login <registry_domain>
       Then confirm when done.`
     - Options: `Done, I logged in` / `Skip push`
   - If user skips, skip this step entirely.

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

Set `KUBECONFIG=<path>` for all kubectl commands in this step.

Apply manifests in order:
```bash
export KUBECONFIG=<path>
kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/secret.yaml
kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/csi-driver.yaml
kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/rbac.yaml
kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/controller.yaml
kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/node.yaml
```

If any `kubectl apply` fails, report the error and **stop**.

After all applies succeed, wait briefly for pods to start:
```bash
sleep 5
```

### Step 7: Verify deployment (if "Verify deployment" selected)

Set `KUBECONFIG=<path>` for all kubectl commands.

Run these checks in sequence:

1. **CSIDriver registration**:
```bash
kubectl get csidriver cinder-iscsi.csi.windriver.com
```
If not found, report and stop.

2. **Controller pod status**:
```bash
kubectl get pods -n kube-system -l app=csi-cinder-iscsi-controllerplugin -o wide
```

3. **Node pod status**:
```bash
kubectl get pods -n kube-system -l app=csi-cinder-iscsi-nodeplugin -o wide
```

4. **Wait for pods** (up to 90 seconds):
```bash
kubectl wait --for=condition=Ready pod -n kube-system -l app=csi-cinder-iscsi-controllerplugin --timeout=90s
kubectl wait --for=condition=Ready pod -n kube-system -l app=csi-cinder-iscsi-nodeplugin --timeout=90s
```

5. **If pods are NOT ready after timeout**, collect diagnostics:
```bash
kubectl describe pod -n kube-system -l app=csi-cinder-iscsi-controllerplugin | tail -30
kubectl logs -n kube-system -l app=csi-cinder-iscsi-controllerplugin -c cinder-iscsi-csi-plugin --tail=30
kubectl describe pod -n kube-system -l app=csi-cinder-iscsi-nodeplugin | tail -30
kubectl logs -n kube-system -l app=csi-cinder-iscsi-nodeplugin -c cinder-iscsi-csi-plugin --tail=30
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
  export KUBECONFIG=<path>
  kubectl delete -f examples/cinder-iscsi-csi-plugin/plugin/
```

## Edge cases

### Build fails — Go version mismatch
Activate `go-version-fix` skill (use `gvm` to switch), then retry the build.

### Build fails — other error
Report the error output and stop. Let the user fix and re-run.

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
| `KUBECONFIG_PATH` | Step 1 prompt | `~/.kube/config-staging` |
| `API_SERVER_URL` | Step 1 prompt | `https://69.167.148.57:6443` |
| `SA_TOKEN` | Step 2 prompt | `eyJhbG...` |

## Rules

- **NEVER modify** `manifests/` or Helm `charts/` — only `examples/cinder-iscsi-csi-plugin/plugin/`.
- **NEVER push to a registry without user confirmation** of the image name + tag.
- **NEVER embed tokens or passwords in skill output** — ask via freeform prompts.
- **ALWAYS validate kubeconfig connectivity** before deploying.
- **ALWAYS check Docker auth** before pushing — prompt only if needed.
- **ALWAYS report a clear summary** at the end with pod status + image in use.
- If any step fails, **stop and report** — do not silently skip.
- Revert manifest image changes if deploy was not selected (to avoid dirty git state).
