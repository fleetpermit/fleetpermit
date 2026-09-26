{{- define "fleetpermit.name" -}}fleetpermit-controller{{- end -}}

{{- /* Cluster-scoped objects are shared by every release, so their names carry
the release name unless it is the default "fleetpermit". */ -}}
{{- define "fleetpermit.clusterName" -}}
{{- if eq .Release.Name "fleetpermit" -}}fleetpermit-controller{{- else -}}{{ printf "%s-fleetpermit-controller" .Release.Name | trunc 63 | trimSuffix "-" }}{{- end -}}
{{- end -}}

{{- define "fleetpermit.labels" -}}
app.kubernetes.io/name: fleetpermit
app.kubernetes.io/component: controller
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "fleetpermit.selectorLabels" -}}
app.kubernetes.io/name: fleetpermit
app.kubernetes.io/component: controller
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "fleetpermit.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}{{ default (include "fleetpermit.name" .) .Values.serviceAccount.name }}{{- else -}}{{ default "default" .Values.serviceAccount.name }}{{- end -}}
{{- end -}}

{{- define "fleetpermit.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.registry -}}{{ printf "%s/%s:%s" .Values.image.registry .Values.image.repository $tag }}{{- else -}}{{ printf "%s:%s" .Values.image.repository $tag }}{{- end -}}
{{- end -}}
