{{- define "foreverbull.validate" -}}
{{- if and .Values.expose.hostPort (gt (int .Values.replicaCount) 1) }}
{{- fail "replicaCount must be 1 when expose.hostPort is true" }}
{{- end }}
{{- /* The broker refuses a TRADING_ENABLED or LOG_LEVEL it cannot read, so
the chart refuses it first with the value name. */}}
{{- if not (kindIs "bool" .Values.broker.trading.enabled) }}
{{- fail (printf "broker.trading.enabled must be true or false, got %q" (toString .Values.broker.trading.enabled)) }}
{{- end }}
{{- if not (has (toString .Values.broker.logLevel) (list "debug" "info" "warning" "error")) }}
{{- fail (printf "broker.logLevel must be debug, info, warning, or error, got %q" (toString .Values.broker.logLevel)) }}
{{- end }}
{{- /* The password is expanded raw into POSTGRES_URL/DATABASE_URL
($(POSTGRES_PASSWORD) in foreverbull.postgresURL); Kubernetes does not
URL-encode it, so a set value with reserved characters breaks the DSN.
Generated (randAlphaNum) and local default values are already safe.
The MinIO secret key gets the same rule, so both set credentials obey
one URL-safe contract. */}}
{{- if and .Values.postgres.password (not (regexMatch "^[-A-Za-z0-9._~]+$" (.Values.postgres.password | toString))) }}
{{- fail "postgres.password must be URL-safe (letters, digits and -._~ are safe)" }}
{{- end }}
{{- if and .Values.minio.secretKey (not (regexMatch "^[-A-Za-z0-9._~]+$" (.Values.minio.secretKey | toString))) }}
{{- fail "minio.secretKey must be URL-safe (letters, digits and -._~ are safe)" }}
{{- end }}
{{- /* Fail fast on a pre-Secret install: an existing Postgres (PVC or
Deployment postgres in the release namespace) with no Secret
foreverbull-credentials yet would otherwise get a fresh generated
password, which Postgres ignores after first init, locking every
client out. lookup is live-cluster only; under helm template all
lookups are empty, so this stays a fresh-install render offline.
Explicit migration is setting all three old values, or opting into
the public defaults with credentials.allowDefaults=true. */}}
{{- $credentialsSecret := lookup "v1" "Secret" .Release.Namespace (include "foreverbull.credentialsSecretName" .) }}
{{- $postgresPVC := lookup "v1" "PersistentVolumeClaim" .Release.Namespace "postgres" }}
{{- $postgresWorkload := lookup "apps/v1" "Deployment" .Release.Namespace "postgres" }}
{{- $existingPostgres := or $postgresPVC $postgresWorkload }}
{{- $allowDefaults := and .Values.credentials .Values.credentials.allowDefaults }}
{{- $allMigrated := and .Values.postgres.password .Values.minio.accessKey .Values.minio.secretKey }}
{{- $migrated := or $allMigrated $allowDefaults }}
{{- include "foreverbull.requireCredentialMigration" (list (not (empty $existingPostgres)) (not (empty $credentialsSecret)) (not (empty $migrated))) }}
{{- /* Remote gRPC TLS reuses the cluster's cert-manager issuer. A missing
host or issuer would render a route that cannot get a certificate. */}}
{{- if .Values.ingress.tls }}
{{- $host := .Values.ingress.host | default "" | toString }}
{{- if or (gt (len $host) 253) (not (regexMatch "^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$" $host)) (regexMatch "^[0-9.]+$" $host) }}
{{- fail (printf "ingress.host must be a DNS name when ingress.tls is true, got %q" $host) }}
{{- end }}
{{- $issuer := .Values.ingress.clusterIssuer | default "" | toString }}
{{- if or (gt (len $issuer) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" $issuer)) }}
{{- fail (printf "ingress.clusterIssuer must name a cert-manager ClusterIssuer when ingress.tls is true, got %q" $issuer) }}
{{- end }}
{{- $entry := .Values.ingress.entryPoint | default "websecure" | toString }}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $entry) }}
{{- fail (printf "ingress.entryPoint must be a Traefik entrypoint name when ingress.tls is true, got %q" $entry) }}
{{- end }}
{{- end }}
{{- end }}

{{- /* Arguments: existingPostgres hasSecret migrated. Split out so
check-chart can call it with synthetic inputs on a copied chart. */}}
{{- define "foreverbull.requireCredentialMigration" -}}
{{- $existingPostgres := index . 0 }}
{{- $hasSecret := index . 1 }}
{{- $migrated := index . 2 }}
{{- if and $existingPostgres (not $hasSecret) (not $migrated) }}
{{- fail "existing Postgres found but Secret foreverbull-credentials does not exist yet, and the old credentials were not migrated: a generated password would not match the password Postgres was initialised with. Migration: (1) on the first upgrade, set postgres.password, minio.accessKey and minio.secretKey to the values the existing install uses, together with credentials.allowDefaults=true if those are the public defaults (foreverbull / minioadmin), and upgrade with Helm (never helm template output); (2) after that upgrade, rotate the password inside Postgres (ALTER USER) and set postgres.password to the new URL-safe value; (3) then remove credentials.allowDefaults=true. Fresh installs need none of this." }}
{{- end }}
{{- end }}

{{- define "foreverbull.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "foreverbull.labels" -}}
helm.sh/chart: {{ include "foreverbull.chart" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: foreverbull
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
platform: foreverbull
{{- end }}

{{- define "foreverbull.selectorLabels" -}}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end }}

{{- define "foreverbull.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default "foreverbull" .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "foreverbull.image" -}}
{{- $repo := index . 0 }}
{{- $tag := index . 1 }}
{{- $defaultTag := index . 2 }}
{{- printf "%s:%s" $repo ($tag | default $defaultTag) }}
{{- end }}

{{- /* $(POSTGRES_PASSWORD) is expanded by Kubernetes from the env var above it. */ -}}
{{- define "foreverbull.postgresURL" -}}
postgres://{{ .Values.postgres.user }}:$(POSTGRES_PASSWORD)@postgres:{{ .Values.postgres.port }}/{{ .Values.postgres.database }}
{{- end }}

{{- define "foreverbull.credentialsSecretName" -}}
foreverbull-credentials
{{- end }}

{{- /*
A password kept in its own Secret. lookup reuses the live value so an
upgrade does not rotate it. helm template has no live Secret, so it
renders a fresh alphanumeric value.
*/ -}}
{{- define "foreverbull.generatedSecret" -}}
{{- $root := index . 0 }}
{{- $name := index . 1 }}
{{- $key := index . 2 }}
{{- $existing := lookup "v1" "Secret" $root.Release.Namespace $name }}
{{- if and $existing $existing.data (hasKey $existing.data $key) }}
{{- index $existing.data $key | b64dec }}
{{- else }}
{{- randAlphaNum 32 }}
{{- end }}
{{- end }}

{{- /*
One credential for Secret foreverbull-credentials. Arguments: list root key
value localDefault generatedDefault secret. generatedDefault "" means random.
expose.hostPort is networking only and never authorises a default:
a set public default, and an empty value resolving to the public
default, both require credentials.allowDefaults=true explicitly
(fbenv passes it for local Kind). Otherwise an empty value is the
value already in the Secret, else generated.
*/ -}}
{{- define "foreverbull.credential" -}}
{{- $root := index . 0 }}
{{- $key := index . 1 }}
{{- $value := index . 2 | default "" | toString }}
{{- $localDefault := index . 3 }}
{{- $generated := index . 4 }}
{{- $secret := index . 5 }}
{{- $allowDefaults := and $root.Values.credentials $root.Values.credentials.allowDefaults }}
{{- if $value }}
{{- if and $secret (eq $value $localDefault) (not $allowDefaults) }}
{{- fail (printf "%s is the public default %q, so anyone who knows the chart could log in. Fix: remove %s from your values (or set it to \"\") to generate one, set your own URL-safe value, or set credentials.allowDefaults=true only for a local or throwaway cluster." $key $value $key) }}
{{- end }}
{{- $value }}
{{- else if $allowDefaults }}
{{- $localDefault }}
{{- else }}
{{- $secretKey := $key | replace "." "-" }}
{{- $existing := lookup "v1" "Secret" $root.Release.Namespace (include "foreverbull.credentialsSecretName" $root) }}
{{- if and $existing $existing.data (hasKey $existing.data $secretKey) }}
{{- index $existing.data $secretKey | b64dec }}
{{- else if $generated }}
{{- $generated }}
{{- else }}
{{- randAlphaNum 32 }}
{{- end }}
{{- end }}
{{- end }}

{{- /*
A tag that starts with sha256: is a digest pin (repository@sha256:…). Any other
tag stays repository:tag, including the immutable sha-<shortsha> registry tag.
*/ -}}
{{- define "foreverbull.appImage" -}}
{{- $repo := index . 0 }}
{{- $tag := index . 1 }}
{{- $defaultTag := index . 2 }}
{{- $prefix := index . 3 }}
{{- $resolved := $tag | default $defaultTag }}
{{- if and $prefix (not (contains "/" $repo)) }}
{{- if hasPrefix "sha256:" $resolved }}
{{- printf "%s/%s@%s" (trimSuffix "/" $prefix) $repo $resolved }}
{{- else }}
{{- printf "%s/%s:%s" (trimSuffix "/" $prefix) $repo $resolved }}
{{- end }}
{{- else }}
{{- if hasPrefix "sha256:" $resolved }}
{{- printf "%s@%s" $repo $resolved }}
{{- else }}
{{- printf "%s:%s" $repo $resolved }}
{{- end }}
{{- end }}
{{- end }}

{{/*
App containers (broker, zipline, grafana). Pass runAsUser.
Kind hostPort publishes only ports above 1024, so local fbenv installs
use this same policy. Postgres, NATS, and MinIO are not included.
Postgres still starts as root so it can prepare the data directory.
*/}}
{{- define "foreverbull.appPodSecurityContext" -}}
runAsNonRoot: true
runAsUser: {{ .runAsUser }}
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{- define "foreverbull.appContainerSecurityContext" -}}
runAsNonRoot: true
runAsUser: {{ .runAsUser }}
allowPrivilegeEscalation: false
capabilities:
  drop:
    - ALL
seccompProfile:
  type: RuntimeDefault
{{- end }}
