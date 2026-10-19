{{- /* atlas.image: the release's image <key>, by digest, from the registry Stowage sets. */ -}}
{{- define "atlas.image" -}}
{{- $img := index .root.Values.stowage.images .key -}}
{{- printf "%s/%s@%s" .root.Values.stowage.registry $img.repository $img.digest -}}
{{- end -}}

{{- /* atlas.labels: a component's selector labels. */ -}}
{{- define "atlas.labels" -}}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end -}}

{{- /* atlas.pod: what every pod of the release shares — the pull Secret `at-stow k8s` records. */ -}}
{{- define "atlas.pod" -}}
{{- with .Values.stowage.pullSecret -}}
imagePullSecrets:
  - name: {{ . }}
{{- end -}}
{{- end -}}

{{- /* atlas.secretEnv: env entries for the keys .keys of the Secret `at-stow k8s` records (its
     secrets and inputs) — each container reads only the keys it uses, as on Docker. A key in
     .optional may be missing. Rendering fails without the Secret. */ -}}
{{- define "atlas.secretEnv" -}}
{{- $optional := .optional | default list -}}
{{- $name := required "stowage.secret: install the chart with at-stow k8s, which records the Secret" .root.Values.stowage.secret -}}
{{- range .keys }}
- name: {{ . }}
  valueFrom:
    secretKeyRef: { name: {{ $name | quote }}, key: {{ . | quote }}{{ if has . $optional }}, optional: true{{ end }} }
{{- end -}}
{{- end -}}

{{- /* atlas.service: a Service <release>-<component> on <port>; headless when .headless (a
     StatefulSet's governing Service). */ -}}
{{- define "atlas.service" -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ .root.Release.Name }}-{{ .component }}
spec:
  {{- if .headless }}
  clusterIP: None
  {{- end }}
  selector:
    {{- include "atlas.labels" . | nindent 4 }}
  ports:
    - port: {{ .port }}
      targetPort: {{ .port }}
{{- end -}}
