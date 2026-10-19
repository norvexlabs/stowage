{{/* The image of a key of stowage.yaml's images, by digest: include "notes.image" (dict "root" $ "key" "web") */}}
{{- define "notes.image" -}}
{{- $img := index .root.Values.stowage.images .key -}}
{{- printf "%s/%s@%s" .root.Values.stowage.registry $img.repository $img.digest -}}
{{- end -}}
