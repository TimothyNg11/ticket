{{/* Image reference for one of our services: ticket-api, ticket-workers, ticket-payments-mock. */}}
{{- define "ticket.image" -}}
{{- $name := printf "ticket-%s" .name -}}
{{- if .root.Values.image.registry -}}
{{ .root.Values.image.registry }}/{{ $name }}:{{ .root.Values.image.tag }}
{{- else -}}
{{ $name }}:{{ .root.Values.image.tag }}
{{- end -}}
{{- end }}

{{- define "ticket.labels" -}}
app.kubernetes.io/part-of: ticket
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/* Pod and container security settings shared by every Go service. */}}
{{- define "ticket.podSecurity" -}}
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  seccompProfile: { type: RuntimeDefault }
automountServiceAccountToken: false
serviceAccountName: ticket
{{- end }}

{{- define "ticket.containerSecurity" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities: { drop: [ALL] }
{{- end }}

{{- define "ticket.dbHost" -}}
{{- if .Values.database.host -}}{{ .Values.database.host }}{{- else -}}postgres{{- end -}}
{{- end }}

{{/* App traffic goes through PgBouncer when it's enabled. */}}
{{- define "ticket.appDbHost" -}}
{{- if .Values.pgbouncer.enabled -}}pgbouncer{{- else -}}{{ include "ticket.dbHost" . }}{{- end -}}
{{- end }}

{{/*
Environment shared by the API and workers. DATABASE_URL is assembled at runtime
from the secret's password via $(POSTGRES_PASSWORD) expansion, so the password
never appears in a ConfigMap or the rendered manifest.
*/}}
{{- define "ticket.env" -}}
- name: POSTGRES_PASSWORD
  valueFrom: { secretKeyRef: { name: {{ .Values.existingSecret }}, key: POSTGRES_PASSWORD } }
- name: JWT_SECRET
  valueFrom: { secretKeyRef: { name: {{ .Values.existingSecret }}, key: JWT_SECRET } }
- name: TICKET_SIGNING_KEY
  valueFrom: { secretKeyRef: { name: {{ .Values.existingSecret }}, key: TICKET_SIGNING_KEY } }
- name: DATABASE_URL
  value: postgres://{{ .Values.database.user }}:$(POSTGRES_PASSWORD)@{{ include "ticket.appDbHost" . }}:5432/{{ .Values.database.name }}?sslmode=disable
- name: REDIS_URL
  value: {{ .Values.redis.url | default "redis://redis:6379/0" | quote }}
- name: PAYMENTS_URL
  value: {{ .Values.payments.url | default "http://payments:8081" | quote }}
- name: ADMIN_EMAILS
  value: {{ .Values.config.adminEmails | quote }}
- name: HOLD_TTL
  value: {{ .Values.config.holdTTL | quote }}
- name: RATE_LIMIT_SCALE
  value: {{ .Values.config.rateLimitScale | quote }}
- name: DRAIN_DELAY
  value: {{ .Values.config.drainDelay | quote }}
- name: TRUST_PROXY
  value: "true"
# Tell the Go runtime its memory budget so the GC works harder before the kernel
# OOM-kills the container: GOMEMLIMIT = the container's memory limit.
- name: MEM_LIMIT_MIB
  valueFrom: { resourceFieldRef: { resource: limits.memory, divisor: 1Mi } }
- name: GOMEMLIMIT
  value: $(MEM_LIMIT_MIB)MiB
{{- if .Values.tracing.enabled }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: http://jaeger:4318
- name: OTEL_TRACES_SAMPLER_ARG
  value: {{ .Values.tracing.sampleRatio | quote }}
{{- end }}
{{- end }}
