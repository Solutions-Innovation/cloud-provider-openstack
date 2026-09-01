{{/*
Render-time guards.

These exist because both conditions are silently dangerous rather than loudly
broken:

  * An empty expectedFsid disables identity check 1 of the pre-publish gate.
    The driver would still run, but it would no longer verify that the mapped
    device belongs to the expected Ceph cluster.

  * exclusive: false would ask for a writable non-exclusive kernel mapping,
    defeating the single-writer guarantee. The driver rejects it at startup, so
    rendering it produces a DaemonSet that crash-loops on a config error. Better
    to fail here, where the message can explain why.

Failing at render time turns both into an immediate, explained error instead of
a subtle loss of safety discovered later.
*/}}

{{- define "cinder-rbd-csi.validate" -}}
{{- if not .Values.driverConfig.rbd.expectedFsid }}
{{- fail "\n\ndriverConfig.rbd.expectedFsid is required.\n\nThe Ceph cluster FSID pins the driver to one cluster; without it the node cannot verify that a mapped device belongs to the expected cluster.\n\nObtain it with:\n  kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph fsid\n\nIt is an environment identifier, not a credential.\n" }}
{{- end }}

{{- if not .Values.driverConfig.rbd.exclusive }}
{{- fail "\n\ndriverConfig.rbd.exclusive must be true.\n\nA writable non-exclusive kernel RBD mapping allows a second writer and defeats the Ceph exclusive-lock guarantee. The driver rejects this at startup, so this chart refuses to render it.\n" }}
{{- end }}

{{- if ne .Values.driverConfig.rbd.mounter "krbd" }}
{{- fail (printf "\n\ndriverConfig.rbd.mounter must be \"krbd\", got %q.\n\nOnly kernel RBD is supported in this release.\n" .Values.driverConfig.rbd.mounter) }}
{{- end }}

{{- if not .Values.cephCredential.secretName }}
{{- fail "\n\ncephCredential.secretName is required.\n\nThe node plugin reads the Ceph key from a projected Secret. The operator duplicates the existing platform client.cinder key into it; see the operator runbook.\n" }}
{{- end }}

{{- if and .Values.cephCredential.create (not .Values.cephCredential.userKey) }}
{{- fail "\n\ncephCredential.create is true but cephCredential.userKey is empty.\n\nSupply the key with --set-file or an out-of-band values file. Never commit a Ceph key to a values file in version control.\n" }}
{{- end }}

{{- if not (or (eq .Values.driverConfig.volume.deleteVolumeMode "retain") (eq .Values.driverConfig.volume.deleteVolumeMode "delete")) }}
{{- fail (printf "\n\ndriverConfig.volume.deleteVolumeMode must be \"retain\" or \"delete\", got %q.\n\nThe driver rejects any other value at startup; a typo such as \"delet\" would otherwise be read as retain.\n" .Values.driverConfig.volume.deleteVolumeMode) }}
{{- end }}
{{- end -}}
