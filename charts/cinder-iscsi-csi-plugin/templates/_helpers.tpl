{{/* vim: set filetype=mustache: */}}
{{/*
Expand the name of the chart.
*/}}
{{- define "cinder-iscsi-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "cinder-iscsi-csi.fullname" -}}
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
{{- define "cinder-iscsi-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels
*/}}
{{- define "cinder-iscsi-csi.labels" -}}
app.kubernetes.io/name: {{ include "cinder-iscsi-csi.name" . }}
helm.sh/chart: {{ include "cinder-iscsi-csi.chart" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: wrc-migration
{{- end -}}

{{/*
Create unified labels for cinder-iscsi-csi components
*/}}
{{- define "cinder-iscsi-csi.common.matchLabels" -}}
app: {{ template "cinder-iscsi-csi.name" . }}
release: {{ .Release.Name }}
{{- end -}}

{{- define "cinder-iscsi-csi.common.metaLabels" -}}
chart: {{ template "cinder-iscsi-csi.chart" . }}
heritage: {{ .Release.Service }}
{{- if .Values.extraLabels }}
{{ toYaml .Values.extraLabels -}}
{{- end }}
{{- end -}}

{{- define "cinder-iscsi-csi.controllerplugin.matchLabels" -}}
component: controllerplugin
{{ include "cinder-iscsi-csi.common.matchLabels" . }}
{{- end -}}

{{- define "cinder-iscsi-csi.controllerplugin.labels" -}}
{{ include "cinder-iscsi-csi.controllerplugin.matchLabels" . }}
{{ include "cinder-iscsi-csi.common.metaLabels" . }}
{{- end -}}

{{- define "cinder-iscsi-csi.controllerplugin.podLabels" -}}
{{ include "cinder-iscsi-csi.controllerplugin.labels" . }}
{{ if .Values.csi.plugin.controllerPlugin.podLabels }}
{{- toYaml .Values.csi.plugin.controllerPlugin.podLabels }}
{{- end }}
{{- end -}}

{{- define "cinder-iscsi-csi.nodeplugin.matchLabels" -}}
component: nodeplugin
{{ include "cinder-iscsi-csi.common.matchLabels" . }}
{{- end -}}

{{- define "cinder-iscsi-csi.nodeplugin.labels" -}}
{{ include "cinder-iscsi-csi.nodeplugin.matchLabels" . }}
{{ include "cinder-iscsi-csi.common.metaLabels" . }}
{{- end -}}

{{- define "cinder-iscsi-csi.nodeplugin.podLabels" -}}
{{ include "cinder-iscsi-csi.nodeplugin.labels" . }}
{{ if .Values.csi.plugin.nodePlugin.podLabels }}
{{- toYaml .Values.csi.plugin.nodePlugin.podLabels }}
{{- end }}
{{- end -}}

{{/*
Common annotations
*/}}
{{- define "cinder-iscsi-csi.annotations" -}}
{{- if .Values.commonAnnotations }}
{{- toYaml .Values.commonAnnotations }}
{{- end }}
{{- end -}}

{{/*
Create unified annotations for cinder-iscsi-csi components
*/}}
{{- define "cinder-iscsi-csi.controllerplugin.podAnnotations" -}}
{{ include "cinder-iscsi-csi.annotations" . }}
{{ if .Values.csi.plugin.controllerPlugin.podAnnotations }}
{{- toYaml .Values.csi.plugin.controllerPlugin.podAnnotations }}
{{- end }}
{{- end -}}

{{- define "cinder-iscsi-csi.nodeplugin.podAnnotations" -}}
{{ include "cinder-iscsi-csi.annotations" . }}
{{ if .Values.csi.plugin.nodePlugin.podAnnotations }}
{{- toYaml .Values.csi.plugin.nodePlugin.podAnnotations }}
{{- end }}
{{- end -}}
