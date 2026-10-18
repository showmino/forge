{{/*
Computed defaults, mirroring deploy/k3s/render.sh's own fallback logic.
*/}}

{{- define "forge.dexGithubOrg" -}}
{{- .Values.dexGithubOrg | default .Values.githubOrg -}}
{{- end -}}

{{- define "forge.writerGroups" -}}
{{- .Values.writerGroups | default (printf "%s:platform-engineering" (include "forge.dexGithubOrg" .)) -}}
{{- end -}}

{{- define "forge.templatesRepo" -}}
{{- .Values.templatesRepo | default (printf "%s/forge-templates" .Values.githubOrg) -}}
{{- end -}}

{{- define "forge.forgeRepo" -}}
{{- .Values.forgeRepo | default (printf "%s/forge" .Values.githubOrg) -}}
{{- end -}}

{{- define "forge.deployRepo" -}}
{{- .Values.deployRepo | default (printf "%s/forge-deploys" .Values.githubOrg) -}}
{{- end -}}

{{- define "forge.chartRepo" -}}
{{- (.Values.chartRepo | default (printf "%s/forge-chart" .Values.githubOrg)) | lower -}}
{{- end -}}

{{- define "forge.dexHost" -}}
{{- if eq .Values.exposure.mode "lb" -}}
{{- printf "dex.%s.nip.io" (.Values.exposure.dexIP | required "exposure.dexIP is required when exposure.mode is \"lb\"") -}}
{{- else -}}
{{- .Values.exposure.dexHost | required "exposure.dexHost is required when exposure.mode is \"cloudLB\"" -}}
{{- end -}}
{{- end -}}

{{- define "forge.apiHost" -}}
{{- if eq .Values.exposure.mode "lb" -}}
{{- printf "forge.%s.nip.io" (.Values.exposure.apiIP | required "exposure.apiIP is required when exposure.mode is \"lb\"") -}}
{{- else -}}
{{- .Values.exposure.apiHost | required "exposure.apiHost is required when exposure.mode is \"cloudLB\"" -}}
{{- end -}}
{{- end -}}

{{- define "forge.dexURL" -}}
{{- if .Values.loginURL -}}
{{- .Values.loginURL -}}
{{- else -}}
{{- printf "http://%s" (include "forge.dexHost" .) -}}
{{- end -}}
{{- end -}}

{{- define "forge.previewBase" -}}
{{- if eq .Values.exposure.mode "lb" -}}
{{- printf "%s.nip.io" (.Values.previews.gatewayIP | required "previews.gatewayIP is required when previews.enabled and exposure.mode is \"lb\"") -}}
{{- else -}}
{{- .Values.previews.gatewayDomain | required "previews.gatewayDomain is required when previews.enabled and exposure.mode is \"cloudLB\"" -}}
{{- end -}}
{{- end -}}

{{/*
forge.webOriginsJSON / forge.webRedirectsJSON: the comma-joined,
double-quoted lists Dex's config.yaml needs for allowedOrigins and the
forge-web client's redirectURIs.
*/}}
{{- define "forge.webOriginsJSON" -}}
{{- $out := list -}}
{{- range .Values.webOrigins -}}
{{- $out = append $out (printf "%q" .) -}}
{{- end -}}
{{- join ", " $out -}}
{{- end -}}

{{- define "forge.webRedirectsJSON" -}}
{{- $out := list -}}
{{- range .Values.webOrigins -}}
{{- $out = append $out (printf "%q" (printf "%s/auth/callback" .)) -}}
{{- end -}}
{{- join ", " $out -}}
{{- end -}}
