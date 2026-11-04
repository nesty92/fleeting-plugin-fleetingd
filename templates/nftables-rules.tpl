table ip {{ .Name }}forwarding;
delete table ip {{ .Name }}forwarding;
{{ if .Instances }}
table ip {{ .Name }}forwarding {
  # Judges only this group's taps: a VM talks to the egress interface and
  # nothing else. Other forwarded traffic on the host (another group,
  # Docker) is left to its own rules.
  chain dropnottap {
    type filter hook forward priority 0; policy accept;

{{ range $instance := .Instances }}
    iifname "{{ $.EgressInterface }}" oifname "{{ $instance.Name }}" counter accept;
    iifname "{{ $instance.Name }}" oifname "{{ $.EgressInterface }}" counter accept;
{{ end }}
{{ range $instance := .Instances }}
    iifname "{{ $instance.Name }}" counter drop;
    oifname "{{ $instance.Name }}" counter drop;
{{ end }}
  }
}
{{ end }}

table netdev {{ .Name }}filter;
delete table netdev {{ .Name }}filter;
{{ if .Instances }}
table netdev {{ .Name }}filter {
{{ range $instance := .Instances }}
  chain {{ $instance.Name }} {
    type filter hook ingress device "{{ $instance.Name }}" priority 0; policy accept;

    ether saddr != "{{ $instance.InstanceTapMacAddress }}" counter drop;
    ip saddr != {{ $instance.InstanceTapIP }} counter drop;

    ip daddr {{ $instance.InstanceGateway }} counter accept;
    ip daddr {{ $.Subnet }}0/24 counter drop;
  }
{{ end }}
}
{{ end }}

table ip {{ .Name }}snat;
delete table ip {{ .Name }}snat;
{{ if .Instances }}
table ip {{ .Name }}snat {
  chain taptonet {
    type nat hook postrouting priority 100;

{{ range $instance := .Instances }}
    iifname {{ $instance.Name }} oifname "{{ $.EgressInterface }}" counter masquerade fully-random;
{{ end }}
  }
}
{{ end }}
