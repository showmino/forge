{{/* The app's environment variables: PORT, DATABASE_URL, run.env (vars) and the
fields of run.secrets. The Deployment and the CronJobs share them. */}}
{{- define "forge.env" -}}
- {name: PORT, value: "{{ .Values.port }}"}
{{- with .Values.database }}
# The connection string CloudNativePG keeps for the service's database.
- name: DATABASE_URL
  valueFrom:
    secretKeyRef: {name: {{ required "database.secret" .secret }}, key: uri}
{{- end }}
{{- range $k, $v := .Values.vars }}
- {name: {{ $k }}, value: {{ $v | quote }}}
{{- end }}
{{- range .Values.secrets }}
{{- $secret := printf "%s-secret" .name }}
{{- range $k, $field := .env }}
- name: {{ $k }}
  valueFrom:
    secretKeyRef: {name: {{ $secret }}, key: {{ $field | quote }}}
{{- end }}
{{- end }}
{{- end }}

{{- /* The Service the address leads to: Forge's own on port 80, or, for a
service that runs a chart, the one chart.expose names. */ -}}
{{- define "forge.backendName" -}}
{{- if .Values.chart -}}{{ required "chart.expose.service" .Values.chart.expose.service }}{{- else -}}{{ .Values.name }}{{- end -}}
{{- end }}

{{- define "forge.backendPort" -}}
{{- if .Values.chart -}}{{ required "chart.expose.port" .Values.chart.expose.port }}{{- else -}}80{{- end -}}
{{- end }}
