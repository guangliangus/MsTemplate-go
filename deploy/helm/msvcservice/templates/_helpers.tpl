{{- define "msvc.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "msvc.fullname" -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "msvc.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/name: {{ include "msvc.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "msvc.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- .Values.serviceAccount.name | default (include "msvc.fullname" .) -}}
{{- else -}}
{{- .Values.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{- define "msvc.api.selectorLabels" -}}
app.kubernetes.io/name: {{ include "msvc.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: api
{{- end -}}

{{- define "msvc.worker.selectorLabels" -}}
app.kubernetes.io/name: {{ include "msvc.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: worker
{{- end -}}

{{- define "msvc.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{/*
The Secret every container reads through envFrom: secret.existingName when the operator keeps it
under another name, else <release>-secrets (rendered by the chart with secret.create, or created
by the operator before the first install).
*/}}
{{- define "msvc.secretName" -}}
{{- .Values.secret.existingName | default (printf "%s-secrets" (include "msvc.fullname" .)) -}}
{{- end -}}

{{/*
True when the chart renders the app Secret itself (secret.create and no existingName): the
deployments then hash it into checksum/secret so a changed value rolls the pods.
*/}}
{{- define "msvc.secretManaged" -}}
{{- if and .Values.secret.create (not .Values.secret.existingName) -}}true{{- end -}}
{{- end -}}

{{/*
The shared container spec: ports, probes, security context, env from the ConfigMap and the Secret.
"port" is the API listener (the api container only); every container has the admin listener, which
serves the probes and /metrics and is never behind the ingress.
*/}}
{{- define "msvc.container" -}}
image: {{ include "msvc.image" .root | quote }}
imagePullPolicy: {{ .root.Values.image.pullPolicy }}
args: [{{ .command | quote }}]
securityContext:
  {{- toYaml .root.Values.containerSecurityContext | nindent 2 }}
ports:
  {{- if .port }}
  - name: http
    containerPort: {{ .port }}
  {{- end }}
  - name: admin
    containerPort: {{ .root.Values.adminPort }}
envFrom:
  - configMapRef:
      name: {{ include "msvc.fullname" .root }}-config
  - secretRef:
      name: {{ include "msvc.secretName" .root }}
  {{- range .root.Values.secret.optionalRefs }}
  - secretRef:
      name: {{ . }}
      optional: true
  {{- end }}
env:
  {{- if .port }}
  - name: SERVER__ADDR
    value: ":{{ .port }}"
  {{- end }}
  - name: SERVER__ADMIN_ADDR
    value: ":{{ .root.Values.adminPort }}"
  {{- range $k, $v := .extraEnv }}
  - name: {{ $k }}
    value: {{ $v | quote }}
  {{- end }}
  {{- $memLimit := dig "limits" "memory" "" .resources }}
  {{- if and .root.Values.runtime.goMemLimitFromResources $memLimit (not (hasKey .root.Values.config "GOMEMLIMIT")) (not (hasKey .extraEnv "GOMEMLIMIT")) }}
  {{- $goMemLimit := include "msvc.gomemlimit" (dict "limit" $memLimit "percent" .root.Values.runtime.goMemLimitPercent) }}
  {{- if $goMemLimit }}
  - name: GOMEMLIMIT
    value: {{ $goMemLimit | quote }}
  {{- end }}
  {{- end }}
startupProbe:
  httpGet: { path: {{ .root.Values.probes.startup.path }}, port: admin }
  periodSeconds: {{ .root.Values.probes.startup.periodSeconds }}
  timeoutSeconds: {{ .root.Values.probes.startup.timeoutSeconds }}
  failureThreshold: {{ .root.Values.probes.startup.failureThreshold }}
livenessProbe:
  httpGet: { path: {{ .root.Values.probes.liveness.path }}, port: admin }
  periodSeconds: {{ .root.Values.probes.liveness.periodSeconds }}
  timeoutSeconds: {{ .root.Values.probes.liveness.timeoutSeconds }}
  failureThreshold: {{ .root.Values.probes.liveness.failureThreshold }}
readinessProbe:
  httpGet: { path: {{ .root.Values.probes.readiness.path }}, port: admin }
  periodSeconds: {{ .root.Values.probes.readiness.periodSeconds }}
  timeoutSeconds: {{ .root.Values.probes.readiness.timeoutSeconds }}
  failureThreshold: {{ .root.Values.probes.readiness.failureThreshold }}
lifecycle:
  preStop:
    sleep:
      seconds: {{ .root.Values.preStopSleepSeconds }}
resources:
  {{- toYaml .resources | nindent 2 }}
{{- end -}}

{{/*
msvc.gomemlimit renders a GOMEMLIMIT value from a Kubernetes memory quantity and a percentage:
(dict "limit" "512Mi" "percent" 80) -> "409MiB". Binary (Ki/Mi/Gi/Ti), decimal (K/M/G/T) and plain
byte quantities are understood; anything else renders nothing, and the caller then sets no env.
*/}}
{{- define "msvc.gomemlimit" -}}
{{- $limit := toString .limit -}}
{{- $percent := float64 (.percent | default 80) -}}
{{- $num := regexFind "^[0-9.]+" $limit -}}
{{- $unit := trimPrefix $num $limit -}}
{{- $bytes := 0.0 -}}
{{- if and $num (eq $unit "Ki") }}{{ $bytes = mulf (float64 $num) 1024 }}
{{- else if and $num (eq $unit "Mi") }}{{ $bytes = mulf (float64 $num) 1048576 }}
{{- else if and $num (eq $unit "Gi") }}{{ $bytes = mulf (float64 $num) 1073741824 }}
{{- else if and $num (eq $unit "Ti") }}{{ $bytes = mulf (float64 $num) 1099511627776 }}
{{- else if and $num (eq $unit "K") }}{{ $bytes = mulf (float64 $num) 1000 }}
{{- else if and $num (eq $unit "M") }}{{ $bytes = mulf (float64 $num) 1000000 }}
{{- else if and $num (eq $unit "G") }}{{ $bytes = mulf (float64 $num) 1000000000 }}
{{- else if and $num (eq $unit "T") }}{{ $bytes = mulf (float64 $num) 1000000000000 }}
{{- else if and $num (eq $unit "") }}{{ $bytes = float64 $num }}
{{- end -}}
{{- if gt $bytes 0.0 -}}
{{- $mib := divf (mulf $bytes $percent) 104857600 -}}
{{- printf "%dMiB" (int64 (floor $mib)) -}}
{{- end -}}
{{- end -}}
