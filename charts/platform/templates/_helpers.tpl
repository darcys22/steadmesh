{{- define "steadmesh.labels" -}}
app.kubernetes.io/part-of: steadmesh
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "steadmesh.platformURL" -}}
{{- default (printf "http://steadmesh-platform.%s.svc:8080" .Release.Namespace) .Values.controller.platformURL -}}
{{- end -}}

{{- define "steadmesh.securityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop: ["ALL"]
seccompProfile:
  type: RuntimeDefault
{{- end -}}
