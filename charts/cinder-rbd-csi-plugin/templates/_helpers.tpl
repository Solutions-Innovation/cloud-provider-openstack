{{/* vim: set filetype=mustache: */}}
{{/*
Expand the name of the chart.
*/}}
{{- define "cinder-rbd-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "cinder-rbd-csi.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "cinder-rbd-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels
*/}}
{{- define "cinder-rbd-csi.labels" -}}
app.kubernetes.io/name: {{ include "cinder-rbd-csi.name" . }}
helm.sh/chart: {{ include "cinder-rbd-csi.chart" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: wrc-migration
{{- end -}}

{{/*
Create unified labels for cinder-rbd-csi components
*/}}
{{- define "cinder-rbd-csi.common.matchLabels" -}}
app: {{ template "cinder-rbd-csi.name" . }}
release: {{ .Release.Name }}
{{- end -}}

{{- define "cinder-rbd-csi.common.metaLabels" -}}
chart: {{ template "cinder-rbd-csi.chart" . }}
heritage: {{ .Release.Service }}
{{- if .Values.extraLabels }}
{{ toYaml .Values.extraLabels -}}
{{- end }}
{{- end -}}

{{- define "cinder-rbd-csi.controllerplugin.matchLabels" -}}
component: controllerplugin
{{ include "cinder-rbd-csi.common.matchLabels" . }}
{{- end -}}

{{- define "cinder-rbd-csi.controllerplugin.labels" -}}
{{ include "cinder-rbd-csi.controllerplugin.matchLabels" . }}
{{ include "cinder-rbd-csi.common.metaLabels" . }}
{{- end -}}

{{- define "cinder-rbd-csi.controllerplugin.podLabels" -}}
{{ include "cinder-rbd-csi.controllerplugin.labels" . }}
{{ if .Values.csi.plugin.controllerPlugin.podLabels }}
{{- toYaml .Values.csi.plugin.controllerPlugin.podLabels }}
{{- end }}
{{- end -}}

{{- define "cinder-rbd-csi.nodeplugin.matchLabels" -}}
component: nodeplugin
{{ include "cinder-rbd-csi.common.matchLabels" . }}
{{- end -}}

{{- define "cinder-rbd-csi.nodeplugin.labels" -}}
{{ include "cinder-rbd-csi.nodeplugin.matchLabels" . }}
{{ include "cinder-rbd-csi.common.metaLabels" . }}
{{- end -}}

{{- define "cinder-rbd-csi.nodeplugin.podLabels" -}}
{{ include "cinder-rbd-csi.nodeplugin.labels" . }}
{{ if .Values.csi.plugin.nodePlugin.podLabels }}
{{- toYaml .Values.csi.plugin.nodePlugin.podLabels }}
{{- end }}
{{- end -}}

{{/*
Common annotations
*/}}
{{- define "cinder-rbd-csi.annotations" -}}
{{- if .Values.commonAnnotations }}
{{- toYaml .Values.commonAnnotations }}
{{- end }}
{{- end -}}

{{/*
Create unified annotations for cinder-rbd-csi components
*/}}
{{- define "cinder-rbd-csi.controllerplugin.podAnnotations" -}}
{{ include "cinder-rbd-csi.annotations" . }}
{{ if .Values.csi.plugin.controllerPlugin.podAnnotations }}
{{- toYaml .Values.csi.plugin.controllerPlugin.podAnnotations }}
{{- end }}
{{- end -}}

{{- define "cinder-rbd-csi.nodeplugin.podAnnotations" -}}
{{ include "cinder-rbd-csi.annotations" . }}
{{ if .Values.csi.plugin.nodePlugin.podAnnotations }}
{{- toYaml .Values.csi.plugin.nodePlugin.podAnnotations }}
{{- end }}
{{- end -}}

{{/*
Build a backward-compatible cacert values object so upgrades from older
releases that never had .Values.cacert do not fail template rendering.
*/}}
{{- define "cinder-rbd-csi.cacert" -}}
{{- $defaults := dict "enabled" false "source" "hostPath" "mountPath" "/etc/ssl/certs" "filename" "ca-certificates.crt" "hostPath" (dict "path" "/etc/ssl/certs" "type" "Directory") "secret" (dict "name" "cinder-rbd-ca-cert" "optional" false) -}}
{{- toYaml (mergeOverwrite $defaults (.Values.cacert | default dict)) -}}
{{- end -}}
