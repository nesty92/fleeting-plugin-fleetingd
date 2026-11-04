#cloud-config
hostname: {{ .InstanceName }}
{{- if .PackageUpgrade }}
package_update: true
package_upgrade: true
{{- end }}
disable_root: true
ssh_pwauth: false
ssh_authorized_keys:
  - "{{ .SSHAuthorizedPublicKey }}"
runcmd:
  - ufw allow from {{ .Gateway }} proto tcp to any port 22