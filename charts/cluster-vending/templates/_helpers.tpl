{{/*
The release's name prefix: "cluster-vending" for a release named
cluster-vending (the documented install), "<release>-cluster-vending" otherwise,
or fullnameOverride.
*/}}
{{- define "cv.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 50 | trimSuffix "-" -}}
{{- else if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* A component's object name, e.g. cluster-vending-server. */}}
{{- define "cv.name" -}}
{{- printf "%s-%s" (include "cv.fullname" .root) .component -}}
{{- end -}}

{{- define "cv.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" -}}
{{- end -}}

{{/* Labels of every object. */}}
{{- define "cv.labels" -}}
helm.sh/chart: {{ include "cv.chart" .root }}
app.kubernetes.io/name: {{ .root.Chart.Name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: cluster-vending
{{- with .component }}
app.kubernetes.io/component: {{ . }}
{{- end }}
{{- with .root.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* The selector of a component's pods. */}}
{{- define "cv.selectorLabels" -}}
app.kubernetes.io/name: {{ .root.Chart.Name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* The image of the Go binary (serve, controller, live). */}}
{{- define "cv.image" -}}
{{- $img := .Values.image -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $img.repository $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $img.repository (default .Chart.AppVersion $img.tag) -}}
{{- end -}}
{{- end -}}

{{- define "cv.uiImage" -}}
{{- $img := .Values.ui.image -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $img.repository $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $img.repository (default .Chart.AppVersion $img.tag) -}}
{{- end -}}
{{- end -}}

{{/* A component's service account name. */}}
{{- define "cv.serviceAccountName" -}}
{{- $sa := index .root.Values .component "serviceAccount" -}}
{{- default (include "cv.name" .) $sa.name -}}
{{- end -}}

{{/*
The live reader's service account. Fixed: the controller binds exactly
cluster-vending-live, in its own namespace, to the kubeconfig reader role in
each tenant namespace (pkg/reconcile LiveReaderServiceAccount).
*/}}
{{- define "cv.liveServiceAccountName" -}}cluster-vending-live{{- end -}}

{{/*
The ClusterRoles the controller binds in tenant namespaces, by name
(pkg/reconcile KubeconfigReaderRole and UsageHistoryWriterRole): fixed.
*/}}
{{- define "cv.kubeconfigReaderRole" -}}cluster-vending-kubeconfig-reader{{- end -}}
{{- define "cv.usageHistoryWriterRole" -}}cluster-vending-usage-history-writer{{- end -}}

{{/* The rules of a role file copied from config/ (files/rbac, make helm-sync). */}}
{{- define "cv.rules" -}}
{{- $role := .root.Files.Get (printf "files/rbac/%s" .file) | fromYaml -}}
{{- if not $role.rules -}}
{{- fail (printf "files/rbac/%s has no rules (make helm-sync)" .file) -}}
{{- end -}}
{{- toYaml $role.rules -}}
{{- end -}}

{{/* The URL the API server reaches the live reader at. */}}
{{- define "cv.liveURL" -}}
{{- printf "http://%s.%s.svc:%v" (include "cv.name" (dict "root" . "component" "live")) .Release.Namespace .Values.live.service.port -}}
{{- end -}}

{{/* The URL the console's server side reaches the API at. */}}
{{- define "cv.apiBaseURL" -}}
{{- if .Values.ui.apiBaseUrl -}}
{{- .Values.ui.apiBaseUrl -}}
{{- else if .Values.server.enabled -}}
{{- printf "http://%s.%s.svc:%v" (include "cv.name" (dict "root" . "component" "server")) .Release.Namespace .Values.server.service.port -}}
{{- else -}}
{{- fail "ui.enabled without server.enabled needs ui.apiBaseUrl" -}}
{{- end -}}
{{- end -}}

{{/* The platform settings file: platform, with aws.* added as flat keys. */}}
{{- define "cv.platformFile" -}}
{{- $file := deepCopy (default dict .Values.platform) -}}
{{- range $key, $value := dict "aws-management-role-arn" .Values.aws.managementRoleArn "aws-server-role-arn" .Values.aws.serverRoleArn "aws-region" .Values.aws.region "aws-source-role-arn" .Values.aws.sourceRoleArn -}}
{{- if $value -}}
{{- if hasKey $file $key -}}
{{- fail (printf "%s is set twice: in platform and in aws" $key) -}}
{{- end -}}
{{- $_ := set $file $key $value -}}
{{- end -}}
{{- end -}}
{{- toYaml $file -}}
{{- end -}}

{{/* The checksum annotation that restarts pods when the settings file changes. */}}
{{- define "cv.platformChecksum" -}}
checksum/platform: {{ include "cv.platformFile" . | sha256sum }}
{{- end -}}

{{/* Pod placement and the pod security context of a component. */}}
{{- define "cv.podPlacement" -}}
{{- $c := index .root.Values .component -}}
{{- with (default .root.Values.global.nodeSelector $c.nodeSelector) }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with (default .root.Values.global.tolerations $c.tolerations) }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $c.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $c.topologySpreadConstraints }}
topologySpreadConstraints:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $c.priorityClassName }}
priorityClassName: {{ . }}
{{- end }}
{{- with .root.Values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
securityContext:
  {{- toYaml $c.podSecurityContext | nindent 2 }}
{{- end -}}

{{/* Checks of value combinations the schema cannot express. */}}
{{- define "cv.validate" -}}
{{- $auth := default dict (default dict .Values.platform).auth -}}
{{- if and .Values.server.enabled (ne (toString (default "" $auth.mode)) "required") (not .Values.allowInsecureNoAuth) -}}
{{- fail "platform.auth.mode must be required: without it every caller of the API is anonymous and may call every RPC for every tenant (freeze the platform, delete any cluster, read account external IDs). Set platform.auth (mode: required and issuers; docs/architecture.md#authentication), or allowInsecureNoAuth: true on a local/dev cluster only" -}}
{{- end -}}
{{- if and .Values.ui.enabled .Values.ui.ingress.enabled (not .Values.ui.auth.enabled) (not .Values.allowInsecureNoAuth) -}}
{{- fail "ui.ingress.enabled needs ui.auth.enabled: an Ingress would publish the console, and through it the whole API, without login. Turn ui.auth on, or set allowInsecureNoAuth: true on a local/dev cluster only" -}}
{{- end -}}
{{- if and .Values.controller.enabled (gt (int .Values.controller.replicas) 1) (not .Values.controller.leaderElect) -}}
{{- fail "controller.replicas > 1 needs controller.leaderElect: two controllers must not reconcile at once" -}}
{{- end -}}
{{- if and .Values.controller.enabled (not .Values.aws.managementRoleArn) (not (hasKey (default dict .Values.platform) "aws-management-role-arn")) -}}
{{- fail "controller.enabled needs aws.managementRoleArn: the IAM role the Crossplane AWS providers run as (docs/architecture.md#controller-settings)" -}}
{{- end -}}
{{- with .Values.server.serviceAccount.annotations -}}
{{- if and (hasKey . "eks.amazonaws.com/role-arn") $.Values.aws.managementRoleArn (eq (index . "eks.amazonaws.com/role-arn") $.Values.aws.managementRoleArn) -}}
{{- fail "server.serviceAccount must not carry the management role: the server makes no AWS change (give it its own server role, aws.serverRoleArn)" -}}
{{- end -}}
{{- end -}}
{{- if hasKey .Values.live.serviceAccount "annotations" -}}
{{- if hasKey (default dict .Values.live.serviceAccount.annotations) "eks.amazonaws.com/role-arn" -}}
{{- fail "live.serviceAccount must not have an IAM role: the live reader makes no AWS call" -}}
{{- end -}}
{{- end -}}
{{- end -}}
