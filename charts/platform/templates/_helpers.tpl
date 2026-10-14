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

{{- define "steadmesh.representativeGateway.validate" -}}
{{- $gateway := .Values.representativeGateway -}}
{{- if and $gateway.ingress.enabled (not $gateway.enabled) -}}
{{- fail "representativeGateway.enabled must be true when representativeGateway.ingress.enabled" -}}
{{- end -}}
{{- if $gateway.enabled -}}
{{- $_ := required "representativeGateway.existingSecret is required when representativeGateway.enabled" $gateway.existingSecret -}}
{{- if not (regexMatch "^[0-9]+$" (toString $gateway.port)) -}}
{{- fail "representativeGateway.port must be an integer between 1024 and 65535, other than 8080" -}}
{{- end -}}
{{- $port := int $gateway.port -}}
{{- if or (lt $port 1024) (gt $port 65535) (eq $port 8080) -}}
{{- fail "representativeGateway.port must be between 1024 and 65535 and cannot use the internal platform port 8080" -}}
{{- end -}}
{{- if $gateway.ingress.enabled -}}
{{- $_ := required "representativeGateway.ingress.host is required when ingress.enabled" $gateway.ingress.host -}}
{{- $_ := required "representativeGateway.ingress.tlsSecretName is required when ingress.enabled; HTTPS is mandatory" $gateway.ingress.tlsSecretName -}}
{{- end -}}
{{- end -}}
{{- end -}}
