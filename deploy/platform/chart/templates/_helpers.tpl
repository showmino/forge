{{- define "forge.org" -}}
{{- .Values.github.org | required "github.org is required" -}}
{{- end -}}

{{- define "forge.host" -}}
{{- .Values.host | required "host is required: the address Forge is reached at, for example forge.example.com" -}}
{{- end -}}

{{- define "forge.apiHost" -}}
{{- .Values.api.host | default (include "forge.host" .) -}}
{{- end -}}

{{- define "forge.loginHost" -}}
{{- .Values.login.host | default (include "forge.host" .) -}}
{{- end -}}

{{- define "forge.scheme" -}}
{{- if .Values.https }}https{{ else }}http{{ end -}}
{{- end -}}

{{- define "forge.webURL" -}}
{{- printf "%s://%s" (include "forge.scheme" .) (include "forge.host" .) -}}
{{- end -}}

{{- define "forge.apiURL" -}}
{{- printf "%s://%s" (include "forge.scheme" .) (include "forge.apiHost" .) -}}
{{- end -}}

{{- define "forge.issuer" -}}
{{- printf "%s://%s/dex" (include "forge.scheme" .) (include "forge.loginHost" .) -}}
{{- end -}}

{{- define "forge.adminTeams" -}}
{{- .Values.github.adminTeams | default (printf "%s:platform-engineering" (include "forge.org" .)) -}}
{{- end -}}

{{- define "forge.templatesRepo" -}}
{{- .Values.github.templatesRepo | default (printf "%s/forge-templates" (include "forge.org" .)) -}}
{{- end -}}

{{- define "forge.deployRepo" -}}
{{- .Values.deploys.repo | default (printf "%s/forge-deploys" (include "forge.org" .)) -}}
{{- end -}}

{{- define "forge.apiImage" -}}
{{- printf "%s:%s" .Values.api.image.repository (.Values.api.image.tag | default (printf "v%s" .Chart.AppVersion)) -}}
{{- end -}}

{{- define "forge.labels" -}}
app.kubernetes.io/part-of: forge
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/*
The API's paths on the shared host: one per ConnectRPC service, because an
Ingress or HTTPRoute prefix matches whole path segments ("/forge.v1." would
match nothing). TestChartRoutesEveryService (internal/server) fails when a
service is missing here.
*/}}
{{- define "forge.apiPaths" -}}
- /forge.v1.AuditService
- /forge.v1.AuthService
- /forge.v1.ClusterService
- /forge.v1.DeployService
- /forge.v1.DocsService
- /forge.v1.EnvironmentService
- /forge.v1.OperationService
- /forge.v1.ResourceService
- /forge.v1.ServiceCatalogService
- /forge.v1.TeamService
- /forge.v1.TemplateService
- /cli
{{- end -}}

{{/*
forge.routes: the hosts and, for each, the paths and the Service behind them,
as YAML: {<host>: [{path, service}]}. The Ingress and the HTTPRoutes are
both made from it.
*/}}
{{- define "forge.routes" -}}
{{- $routes := dict -}}
{{- if .Values.web.enabled -}}
{{- $_ := set $routes (include "forge.host" .) (list (dict "path" "/" "service" "forge-web" "port" 80)) -}}
{{- end -}}
{{- $api := include "forge.apiHost" . -}}
{{- range (include "forge.apiPaths" . | fromYamlArray) -}}
{{- $_ := set $routes $api (append (get $routes $api | default list) (dict "path" . "service" "forge-api" "port" 80)) -}}
{{- end -}}
{{- $login := include "forge.loginHost" . -}}
{{- $_ := set $routes $login (append (get $routes $login | default list) (dict "path" "/dex" "service" "dex" "port" 80)) -}}
{{- toYaml $routes -}}
{{- end -}}

{{- define "forge.dbSecret" -}}
{{- if eq .Values.database.mode "external" -}}
{{- .Values.database.external.existingSecret | required "database.external.existingSecret is required with database.mode external" -}}
{{- else if eq .Values.database.mode "cloudnativepg" -}}
forge-db-app
{{- else -}}
{{- .Values.database.bundled.existingSecret | default "forge-db" -}}
{{- end -}}
{{- end -}}
