#cloud-config
# The VM a snapshot is taken of: like any VM, plus the agent that gives
# each restored copy its own address, key and identity.
hostname: {{ .InstanceName }}
{{- if .PackageUpgrade }}
package_update: true
package_upgrade: true
{{- end }}
disable_root: true
ssh_pwauth: false
ssh_authorized_keys:
  - "{{ .SSHAuthorizedPublicKey }}"
write_files:
  - path: /etc/systemd/system/fleetingd-agent.socket
    content: |
      [Socket]
      ListenStream=vsock::52
      Accept=yes
      [Install]
      WantedBy=sockets.target
  - path: /etc/systemd/system/fleetingd-agent@.service
    content: |
      [Service]
      ExecStart=/bin/sh
      StandardInput=socket
      StandardOutput=socket
      StandardError=socket
runcmd:
  - ufw allow from {{ .Subnet }}0/24 proto tcp to any port 22
  - systemctl daemon-reload
  - systemctl enable --now fleetingd-agent.socket
